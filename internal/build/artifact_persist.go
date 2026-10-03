package build

// artifact_persist.go 把构建产出的 jar 文件 / dist 目录的**真字节**拷进制品库(Story 8-16 / FR-8-16),
// 使产物在临时工作区销毁后仍可被部署取用。无制品库注入(b.artStore==nil)时保持旧行为(reference=文件名,
// 占位),向后兼容。
//
//   - jar : 文件原样字节 → store.Put → reference=storeKey;metadata 记 filename(部署落盘名)、format=file。
//   - dist: 目录打成 tar.gz 字节流 → store.Put → reference=storeKey;metadata 记 format=tar.gz(部署远端解包)。
//
// 制品库句柄(sha256)落入 run_artifacts.reference;部署侧据 metadata.stored 判定走「取真字节上传」而非占位。

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/huangchengsir/pipewright/internal/run"
)

// storeJarBytes 把 jar/单文件字节存入制品库,改写 art.Reference 为存储句柄并补 metadata。
// rename 非空 → metadata.filename 记为 rename(部署落盘名;此前固定原文件名,现显式可配)。
// store 为 nil 或存储失败 → 不改 art(保持旧占位行为),返回是否已存。
func (b *Builder) storeJarBytes(art *run.Artifact, jarPath, rename string, onLine func(stream, line string)) bool {
	if b.artStore == nil {
		return false
	}
	f, err := os.Open(jarPath)
	if err != nil {
		onLine(streamStderr, "制品库:打开 jar 失败(降级占位):"+err.Error())
		return false
	}
	defer func() { _ = f.Close() }()

	key, size, err := b.artStore.Put(f)
	if err != nil {
		onLine(streamStderr, "制品库:存 jar 失败(降级占位):"+err.Error())
		return false
	}
	filename := filepath.Base(jarPath)
	if rename = strings.TrimSpace(rename); rename != "" {
		filename = rename
	}
	art.Reference = key
	art.SizeBytes = size
	if art.Metadata == nil {
		art.Metadata = map[string]any{}
	}
	art.Metadata["stored"] = true
	art.Metadata["format"] = "file"
	art.Metadata["filename"] = filename
	onLine(streamStdout, "制品库:已归档 "+filename+"(句柄 "+key[:12]+"…)")
	return true
}

// storeDistDir 把 dist 目录归档存入制品库,改写 art.Reference 并补 metadata。
// 打包行为由节点配置显式决定(此前硬编码「一律 tar.gz + 固定去顶层目录」):
//   - packMode == "none":不打包 —— 逐文件存库(内容寻址去重),metadata 记 format=files +
//     files 清单(相对路径 / 句柄 / 大小);部署端按清单逐文件上传,目录结构原样保留。
//   - 否则(含空 = 默认):打成 tar.gz;layout == "top" 保留顶层目录(解包后文件在 <dir>/ 下),
//     layout == "contents"(默认 = 旧行为)去掉顶层目录(内容铺包根)。
//
// store 为 nil 或失败 → 不改 art(保持旧占位)。tar 用流式管道,大目录不占额外内存/磁盘临时。
func (b *Builder) storeDistDir(art *run.Artifact, dirPath, packMode, layout string, onLine func(stream, line string)) bool {
	if b.artStore == nil {
		return false
	}
	if packMode == "none" {
		return b.storeDirFiles(art, dirPath, onLine)
	}
	keepTop := layout == "top"
	pr, pw := io.Pipe()
	go func() {
		// 把 dirPath 下内容打成 tar.gz 写入管道(出错经 CloseWithError 传到读端)。
		pw.CloseWithError(tarGzDir(dirPath, pw, keepTop))
	}()

	key, size, err := b.artStore.Put(pr)
	_ = pr.Close()
	if err != nil {
		onLine(streamStderr, "制品库:存 dist(tar.gz)失败(降级占位):"+err.Error())
		return false
	}
	art.Reference = key
	art.SizeBytes = size
	if art.Metadata == nil {
		art.Metadata = map[string]any{}
	}
	art.Metadata["stored"] = true
	art.Metadata["format"] = "tar.gz"
	if keepTop {
		art.Metadata["tarLayout"] = "top"
	}
	base := filepath.Base(dirPath)
	if keepTop {
		onLine(streamStdout, "制品库:已归档目录 → tar.gz(含顶层目录 "+base+"/,解包后文件在 <release>/"+base+"/ 下;句柄 "+key[:12]+"…)")
	} else {
		onLine(streamStdout, "制品库:已归档目录 → tar.gz(内容铺包根,不含 "+base+"/ 前缀;句柄 "+key[:12]+"…)")
	}
	return true
}

// storeDirFiles 把目录**不打包**逐文件归档:每个普通文件独立存库(内容寻址,相同内容跨 run 去重),
// metadata 记 format=files + files 清单([{path,key,size}],path 为相对斜杠路径)。
// 部署端按清单逐文件上传到 <release>/<path>,目录结构原样;跨阶段 restore 同样按清单还原。
// Reference 记首文件句柄(合法句柄;完整清单以 metadata.files 为准)。
func (b *Builder) storeDirFiles(art *run.Artifact, dirPath string, onLine func(stream, line string)) bool {
	type fileEntry struct {
		Path string `json:"path"`
		Key  string `json:"key"`
		Size int64  `json:"size"`
	}
	var files []fileEntry
	var total int64
	firstKey := ""
	walkErr := filepath.Walk(dirPath, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return nil // 目录/符号链接等不入清单(与 tar 打包同口径:仅普通文件)
		}
		rel, rerr := filepath.Rel(dirPath, p)
		if rerr != nil {
			return rerr
		}
		f, oerr := os.Open(p)
		if oerr != nil {
			return oerr
		}
		key, size, perr := b.artStore.Put(f)
		_ = f.Close()
		if perr != nil {
			return perr
		}
		if firstKey == "" {
			firstKey = key
		}
		files = append(files, fileEntry{Path: filepath.ToSlash(rel), Key: key, Size: size})
		total += size
		return nil
	})
	if walkErr != nil {
		onLine(streamStderr, "制品库:按文件清单归档失败(降级占位):"+walkErr.Error())
		return false
	}
	// 空目录(无普通文件)同样归档为「空清单」而非降级占位:与 tar 路径一致(空目录会归档成
	// 合法空 tar.gz)。降级占位会把 reference 字符串当产物字节写到目标机,比空目录更糟。
	ref := firstKey
	if ref == "" {
		k, _, perr := b.artStore.Put(strings.NewReader(""))
		if perr != nil {
			onLine(streamStderr, "制品库:空目录归档失败(降级占位):"+perr.Error())
			return false
		}
		ref = k
	}
	art.Reference = ref
	art.SizeBytes = total
	if art.Metadata == nil {
		art.Metadata = map[string]any{}
	}
	art.Metadata["stored"] = true
	art.Metadata["format"] = "files"
	// files 初始化为空切片(非 nil):空目录也要序列化成 "[]" 而非 "null",
	// 否则读侧 decodeFilesManifest 会把 null 判成「缺少清单」。
	if files == nil {
		files = []fileEntry{}
	}
	raw, _ := json.Marshal(files)
	art.Metadata["files"] = json.RawMessage(raw)
	if len(files) == 0 {
		onLine(streamStdout, "制品库:目录内无普通文件,已归档空清单(句柄 "+ref[:12]+"…)")
	} else {
		onLine(streamStdout, fmt.Sprintf("制品库:已按文件清单归档(%d 个文件,目录结构保留;首句柄 %s…)", len(files), ref[:12]))
	}
	return true
}

// tarGzDir 把 root 目录下的内容打成 tar.gz 写入 w。
// keepTop=false(默认):不打 root 自身这层,包内路径相对 root(内容铺包根,旧行为);
// keepTop=true:包内路径以 root 的基名开头(保留顶层目录,解包后文件在 <basename>/ 下)。
// 仅打普通文件与目录(跳过符号链接等特殊文件,避免逃逸/不可移植);路径用相对的斜杠路径。
func tarGzDir(root string, w io.Writer, keepTop bool) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	walkErr := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if p == root && !keepTop {
			return nil // 不打 root 自身,只打其内容
		}
		rel, rerr := filepath.Rel(filepath.Dir(root), p)
		if rerr != nil {
			return rerr
		}
		if !keepTop {
			if rel, rerr = filepath.Rel(root, p); rerr != nil {
				return rerr
			}
		}
		// 只处理普通文件与目录(符号链接/设备等跳过,防逃逸 + 跨平台可移植)。
		if !fi.Mode().IsRegular() && !fi.IsDir() {
			return nil
		}
		hdr, herr := tar.FileInfoHeader(fi, "")
		if herr != nil {
			return herr
		}
		hdr.Name = filepath.ToSlash(rel)
		if fi.IsDir() {
			hdr.Name += "/"
		}
		if werr := tw.WriteHeader(hdr); werr != nil {
			return werr
		}
		if fi.IsDir() {
			return nil
		}
		f, oerr := os.Open(p)
		if oerr != nil {
			return oerr
		}
		defer func() { _ = f.Close() }()
		_, cerr := io.Copy(tw, f)
		return cerr
	})

	// 先关 tar/gz 把缓冲刷净,再返回(walk 错误优先)。
	if cerr := tw.Close(); cerr != nil && walkErr == nil {
		walkErr = cerr
	}
	if cerr := gz.Close(); cerr != nil && walkErr == nil {
		walkErr = cerr
	}
	return walkErr
}
