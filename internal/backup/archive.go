// Package backup 实现平台自身数据的备份与恢复。
//
// 备份把「重建平台所需的最小数据集」打包为单文件归档(.pwb,内为 tar.gz):
//   - 数据库快照:SQLite 用 VACUUM INTO(在线一致快照,含表/触发器/数据);
//     MySQL 用纯 Go SQL dump(无外部二进制依赖,回放见 dump_mysql.go);
//   - acme/(CA 账号密钥,丢失后无法再签发续期);
//   - artifacts/ 制品库(可选,量大默认不含);
//   - master key(可选;仅当用户提供口令时随包携带,经 argon2id(口令)→secretbox
//     密封——包内绝无明文 key,库内凭据本就是密文,不含口令时包不携带任何机密)。
//
// 恢复入口有二,共用同一恢复核心(ApplyRestore):
//   - CLI:pipewright restore <archive>(停服离线,main 在打开存储前分派);
//   - UI 在线:上传→校验→暂存+pending 标记→空闲后自重启,启动期应用(restore.go)。
package backup

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// 归档内固定路径(正斜杠,与 tar 头一致)。
const (
	manifestPath  = "manifest.json"
	dbSQLitePath  = "data/db.sqlite"
	dbMySQLPath   = "data/db.sql"
	acmeDir       = "acme"
	artifactsDir  = "artifacts"
	masterKeyPath = "master.key"
)

// ManifestFormat 当前归档格式版本。
const ManifestFormat = 1

// Contains 声明归档里实际包含的数据面。
type Contains struct {
	DB        bool `json:"db"`
	Acme      bool `json:"acme"`
	Artifacts bool `json:"artifacts"`
	// MasterKeySealed 表示包含经口令密封的 master.key(读取需恢复口令)。
	MasterKeySealed bool `json:"masterKeySealed"`
}

// Manifest 是归档根部的清单:校验和 + 内容声明,恢复前据此验证。
type Manifest struct {
	Format          int         `json:"format"`
	CreatedAt       string      `json:"createdAt"`
	PlatformVersion string      `json:"platformVersion"`
	DBDriver        string      `json:"dbDriver"` // sqlite|mysql
	Files           []FileEntry `json:"files"`
	Contains        Contains    `json:"contains"`
}

// FileEntry 是归档内一个文件的元信息(Name 为归档内正斜杠路径)。
type FileEntry struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Sha256 string `json:"sha256"`
}

// ErrBadArchive 表示归档不可读/不完整/校验失败。
var ErrBadArchive = errors.New("backup: bad archive")

// readManifest 从归档读出清单(不做全量校验;完整校验见 validateArchive)。
func readManifest(archivePath string) (*Manifest, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, fmt.Errorf("%w: open: %v", ErrBadArchive, err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("%w: gzip: %v", ErrBadArchive, err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("%w: manifest.json missing", ErrBadArchive)
		}
		if err != nil {
			return nil, fmt.Errorf("%w: tar: %v", ErrBadArchive, err)
		}
		if hdr.Typeflag == tar.TypeReg && hdr.Name == manifestPath {
			raw, err := io.ReadAll(io.LimitReader(tr, 1<<20))
			if err != nil {
				return nil, fmt.Errorf("%w: read manifest: %v", ErrBadArchive, err)
			}
			var m Manifest
			if err := json.Unmarshal(raw, &m); err != nil {
				return nil, fmt.Errorf("%w: manifest json: %v", ErrBadArchive, err)
			}
			if m.Format != ManifestFormat {
				return nil, fmt.Errorf("%w: format %d unsupported", ErrBadArchive, m.Format)
			}
			return &m, nil
		}
	}
}

// ValidateArchive 校验归档完整性:manifest 可读、每个文件 sha256 匹配、无未登记文件。
// 返回清单供调用方展示内容面。
func ValidateArchive(archivePath string) (*Manifest, error) {
	m, err := readManifest(archivePath)
	if err != nil {
		return nil, err
	}
	registered := make(map[string]FileEntry, len(m.Files))
	for _, e := range m.Files {
		registered[e.Name] = e
	}
	if m.Contains.DB {
		want := dbSQLitePath
		if m.DBDriver == "mysql" {
			want = dbMySQLPath
		}
		if _, ok := registered[want]; !ok {
			return nil, fmt.Errorf("%w: db snapshot missing", ErrBadArchive)
		}
	}
	if m.Contains.MasterKeySealed {
		if _, ok := registered[masterKeyPath]; !ok {
			return nil, fmt.Errorf("%w: master.key missing", ErrBadArchive)
		}
	}

	f, err := os.Open(archivePath)
	if err != nil {
		return nil, fmt.Errorf("%w: open: %v", ErrBadArchive, err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("%w: gzip: %v", ErrBadArchive, err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	seen := map[string]bool{manifestPath: true}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: tar: %v", ErrBadArchive, err)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeDir {
			return nil, fmt.Errorf("%w: unexpected entry type %q", ErrBadArchive, hdr.Name)
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		name := path.Clean(hdr.Name)
		name = strings.TrimPrefix(name, "./")
		if name == manifestPath {
			continue
		}
		e, ok := registered[name]
		if !ok {
			return nil, fmt.Errorf("%w: unregistered file %q", ErrBadArchive, name)
		}
		if seen[name] {
			return nil, fmt.Errorf("%w: duplicate file %q", ErrBadArchive, name)
		}
		seen[name] = true
		h := sha256.New()
		n, err := io.Copy(h, io.LimitReader(tr, e.Size+1))
		if err != nil {
			return nil, fmt.Errorf("%w: hash %q: %v", ErrBadArchive, name, err)
		}
		if n != e.Size || hex.EncodeToString(h.Sum(nil)) != e.Sha256 {
			return nil, fmt.Errorf("%w: checksum mismatch %q", ErrBadArchive, name)
		}
	}
	for name := range registered {
		if !seen[name] {
			return nil, fmt.Errorf("%w: missing file %q", ErrBadArchive, name)
		}
	}
	return m, nil
}

// archiveBuilder 增量构建归档:边写边算校验和,Close 落 manifest。
type archiveBuilder struct {
	f  *os.File
	gz *gzip.Writer
	tr *tar.Writer
	m  Manifest
}

// newArchiveBuilder 在 targetPath 创建归档(temp 写入 + 原子 rename 由调用方安排:
// 此处直接写最终路径的 .part 同伴文件)。
func newArchiveBuilder(partPath, platformVersion, dbDriver string) (*archiveBuilder, error) {
	f, err := os.Create(partPath)
	if err != nil {
		return nil, fmt.Errorf("backup: create archive: %w", err)
	}
	gz := gzip.NewWriter(f)
	tr := tar.NewWriter(gz)
	return &archiveBuilder{
		f:  f,
		gz: gz,
		tr: tr,
		m: Manifest{
			Format:          ManifestFormat,
			PlatformVersion: platformVersion,
			DBDriver:        dbDriver,
			Files:           nil,
		},
	}, nil
}

// addFile 把本地文件以归档名 name 加入(校验和登记进 manifest)。
func (b *archiveBuilder) addFile(name, localPath string) error {
	info, err := os.Stat(localPath)
	if err != nil {
		return fmt.Errorf("backup: stat %s: %w", localPath, err)
	}
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return fmt.Errorf("backup: tar header %s: %w", localPath, err)
	}
	hdr.Name = name
	hdr.Mode = 0o600
	if err := b.tr.WriteHeader(hdr); err != nil {
		return fmt.Errorf("backup: tar write header %s: %w", name, err)
	}
	sum, n, err := fileSha256(localPath)
	if err != nil {
		return fmt.Errorf("backup: hash %s: %w", localPath, err)
	}
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("backup: open %s: %w", localPath, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(b.tr, f); err != nil {
		return fmt.Errorf("backup: tar copy %s: %w", name, err)
	}
	b.m.Files = append(b.m.Files, FileEntry{Name: name, Size: n, Sha256: sum})
	return nil
}

// addBytes 把一段字节以归档名 name 加入。
func (b *archiveBuilder) addBytes(name string, data []byte) error {
	hdr := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg}
	if err := b.tr.WriteHeader(hdr); err != nil {
		return fmt.Errorf("backup: tar write header %s: %w", name, err)
	}
	if _, err := b.tr.Write(data); err != nil {
		return fmt.Errorf("backup: tar write %s: %w", name, err)
	}
	sum := sha256.Sum256(data)
	b.m.Files = append(b.m.Files, FileEntry{
		Name: name, Size: int64(len(data)), Sha256: hex.EncodeToString(sum[:]),
	})
	return nil
}

// addDir 递归把目录下全部常规文件加入(prefix 为归档内目录前缀,正斜杠)。
func (b *archiveBuilder) addDir(prefix, localDir string) error {
	return filepath.WalkDir(localDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil // 目录不存在 = 该数据面缺席,静默跳过
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, serr := d.Info()
		if serr != nil || !info.Mode().IsRegular() {
			return nil // 跳过符号链接/套接字等非常规文件
		}
		rel, rerr := filepath.Rel(localDir, p)
		if rerr != nil {
			return rerr
		}
		return b.addFile(prefix+"/"+filepath.ToSlash(rel), p)
	})
}

// Close 写 manifest 并收尾(不关闭底层错误时的清理,由调用方 Remove part 文件)。
func (b *archiveBuilder) Close(createAt string, contains Contains) error {
	b.m.CreatedAt = createAt
	b.m.Contains = contains
	raw, err := json.MarshalIndent(&b.m, "", "  ")
	if err != nil {
		return fmt.Errorf("backup: marshal manifest: %w", err)
	}
	if err := b.addBytes(manifestPath, raw); err != nil {
		return err
	}
	if err := b.tr.Close(); err != nil {
		return fmt.Errorf("backup: close tar: %w", err)
	}
	if err := b.gz.Close(); err != nil {
		return fmt.Errorf("backup: close gzip: %w", err)
	}
	return b.f.Close()
}

// abort 关闭并删除半成品归档。
func (b *archiveBuilder) abort() {
	_ = b.tr.Close()
	_ = b.gz.Close()
	_ = b.f.Close()
}

// fileSha256 返回文件 sha256 十六进制与大小。
func fileSha256(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
