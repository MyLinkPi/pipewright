package build

// artifact_restore.go 实现**跨阶段产物传递**:把本 run 此前阶段已归档的「文件类产物」(jar/dist/
// archive)真字节从制品库恢复到当前阶段的新工作区,落回各自原始工作区相对路径(产物登记时记的
// workspacePath)。
//
// 背景:每个阶段独立 clone 一份临时工作区、用完即销(并行安全、宿主零落地),阶段间不共享工作区。
// 因此把「构建(出 backend/target/x.jar)」与「打包镜像(Dockerfile COPY target/x.jar)」拆成不同
// 阶段时,下游阶段克隆出的是纯源码、没有上游产的 jar → docker build 的 COPY 找不到源而失败。本文件
// 在下游阶段克隆后、执行 job 前,把上游已归档的文件产物按原相对路径放回工作区,补齐这条「产物传递」。
//
// best-effort:制品库 / lister 未注入、无上游产物、单条恢复失败都不阻断本阶段(仅记日志);首个阶段
// 无上游产物天然 no-op。只恢复 stored=true 且记了 workspacePath 的文件类产物;镜像类产物(无字节、
// 经 registry 流转)无 workspacePath 故跳过。

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/huangchengsir/pipewright/internal/dagrun"
	"github.com/huangchengsir/pipewright/internal/run"
)

// restorePriorArtifacts 恢复本 run 上游阶段的文件产物到 workspace(见文件头）。
func (b *Builder) restorePriorArtifacts(ctx context.Context, r *run.Run, workspace string, rep dagrun.StageReporter) {
	if b.artStore == nil || b.artifactLister == nil || r == nil {
		return
	}
	arts, err := b.artifactLister(ctx, r.ID)
	if err != nil || len(arts) == 0 {
		return
	}
	onLine := func(stream, line string) { _ = rep.Log(ctx, stream, line) }
	for _, a := range arts {
		if stored, _ := a.Metadata["stored"].(bool); !stored {
			continue // 仅恢复制品库归档的真字节产物(镜像类无字节)
		}
		wp, _ := a.Metadata["workspacePath"].(string)
		wp = strings.TrimSpace(wp)
		if wp == "" {
			continue // 无原始路径(旧产物 / 镜像)→ 无从恢复
		}
		// zip-slip 防护:相对路径必须落在工作区内。
		clean := filepath.Clean(wp)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			onLine(streamStderr, "跨阶段恢复:拒绝越界产物路径 "+wp)
			continue
		}
		dest := filepath.Join(workspace, clean)
		rc, oerr := b.artStore.Open(a.Reference)
		if oerr != nil {
			onLine(streamStderr, "跨阶段恢复:取产物字节失败 "+a.Name+":"+oerr.Error())
			continue
		}
		format, _ := a.Metadata["format"].(string)
		var rerr error
		switch format {
		case "tar.gz":
			rerr = extractTarGzTo(rc, dest) // dist:解包到目录(包内布局由打包侧 layout 决定,原样恢复)
		case "files":
			_ = rc.Close()
			rerr = b.restoreFilesManifest(a, dest, onLine) // 逐文件清单:按清单恢复目录结构
		default:
			rerr = writeFileTo(rc, dest) // jar / 单文件:写文件
		}
		if format == "files" {
			if rerr != nil {
				onLine(streamStderr, "跨阶段恢复:写盘失败 "+a.Name+":"+rerr.Error())
			} else {
				onLine(streamStdout, "已恢复上游产物:"+a.Name+" → "+clean)
			}
			continue
		}
		_ = rc.Close()
		if rerr != nil {
			onLine(streamStderr, "跨阶段恢复:写盘失败 "+a.Name+":"+rerr.Error())
			continue
		}
		onLine(streamStdout, "已恢复上游产物:"+a.Name+" → "+clean)
	}
}

// storedFileEntry 是 format=files 产物清单里的一项(metadata.files 数组元素)。
type storedFileEntry struct {
	Path string `json:"path"`
	Key  string `json:"key"`
	Size int64  `json:"size"`
}

// decodeFilesManifest 从产物 metadata 解出 format=files 的清单。
//
// metadata 有两种形状:构建侧刚写入时 files 是 json.RawMessage,但**经 DB 往返**后
// (run.decodeMetadata 用 json.Unmarshal 到 map[string]any)会变成 []any —— 本函数的调用方
// 经 artifactLister(= run.Service.ListArtifacts)读,拿到的必然是后者。故统一「再序列化 →
// 反序列化」兼容两种形状,绝不用 `.(json.RawMessage)` 断言(断言恒失败 → 清单丢失 → 恢复失败)。
func decodeFilesManifest(md map[string]any) ([]storedFileEntry, error) {
	v, ok := md["files"]
	if !ok || v == nil {
		return nil, fmt.Errorf("产物缺少 files 清单")
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("产物 files 清单非法:%v", err)
	}
	var files []storedFileEntry
	if err := json.Unmarshal(raw, &files); err != nil {
		return nil, fmt.Errorf("产物 files 清单非法:%v", err)
	}
	return files, nil
}

// restoreFilesManifest 按 format=files 产物的清单逐文件恢复到 destDir(保持相对路径目录结构)。
// 单文件失败 → 返回该错误(尽力语义由调用方记日志);zip-slip 防护同上。
func (b *Builder) restoreFilesManifest(a run.Artifact, destDir string, onLine func(stream, line string)) error {
	files, err := decodeFilesManifest(a.Metadata)
	if err != nil {
		return fmt.Errorf("产物 %s %v", a.Name, err)
	}
	for _, fe := range files {
		rel := filepath.Clean("/" + strings.ReplaceAll(fe.Path, "\\", "/"))
		if rel == "/" || rel == "." {
			continue
		}
		dest := filepath.Join(destDir, filepath.FromSlash(rel))
		rc, err := b.artStore.Open(fe.Key)
		if err != nil {
			return fmt.Errorf("取文件 %s 失败:%v", fe.Path, err)
		}
		werr := writeFileTo(rc, dest)
		_ = rc.Close()
		if werr != nil {
			return fmt.Errorf("写文件 %s 失败:%v", fe.Path, werr)
		}
	}
	return nil
}

// writeFileTo 把 r 的内容写到 dest(覆盖),自动建父目录。
func writeFileTo(r io.Reader, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(f, r)
	return err
}

// extractTarGzTo 把 tar.gz 流解包到 destDir(覆盖),含 zip-slip 防护;符号链接/特殊文件跳过。
func extractTarGzTo(r io.Reader, destDir string) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	destAbs, err := filepath.Abs(destDir)
	if err != nil {
		return err
	}
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, terr := tr.Next()
		if errors.Is(terr, io.EOF) {
			break
		}
		if terr != nil {
			return terr
		}
		target := filepath.Join(destAbs, filepath.Clean("/"+hdr.Name))
		if target != destAbs && !strings.HasPrefix(target, destAbs+string(filepath.Separator)) {
			return fmt.Errorf("tar 条目越界: %s", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, oerr := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
			if oerr != nil {
				return oerr
			}
			if _, cerr := io.Copy(f, tr); cerr != nil {
				_ = f.Close()
				return cerr
			}
			_ = f.Close()
		default:
			// 符号链接 / 设备等跳过(跨阶段产物只需普通文件与目录)。
		}
	}
	return nil
}
