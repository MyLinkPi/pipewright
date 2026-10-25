// restore.go:恢复核心 + master key 口令密封 + UI 在线恢复的 pending 标记。
// ApplyRestore 被 CLI(pipewright restore)与启动期(UI 在线恢复的延迟应用)共用,
// 调用时机都保证在 store 打开之前,因此可以安全替换 SQLite 文件 / 重放 MySQL dump。
package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/nacl/secretbox"

	"github.com/huangchengsir/pipewright/internal/config"
)

// RestoreOptions 是恢复的落点参数(由调用方按部署形态给出)。
type RestoreOptions struct {
	Driver    string // sqlite|mysql
	DataDir   string
	DBPath    string // sqlite 库文件路径
	MySQLDSN  string // mysql 模式回放/安全备份用
	Passphrase string
}

// RestoreReport 汇报恢复结果与告警(直出前端/CLI 输出)。
type RestoreReport struct {
	AppliedDB        bool
	DBSafetyPath     string // 恢复前现库的安全备份位置
	AppliedAcme      bool
	AppliedArtifacts bool
	// MasterKeyAction: "written"(已写 master.key.txt)/"present"(包里有但没给口令,
	// 未恢复)/"absent"(包里没有)/"skipped"(恢复选项未要求)。
	MasterKeyAction   string
	MasterKeyFilePath string
	Warnings          []string
}

// ApplyRestore 校验并应用一份备份归档。任一关键步骤失败即返回错误(已完成的
// 替换不回滚——安全备份在手,由管理员决定回退方式)。
func ApplyRestore(archivePath string, opt RestoreOptions) (*RestoreReport, error) {
	m, err := ValidateArchive(archivePath)
	if err != nil {
		return nil, err
	}
	rep := &RestoreReport{MasterKeyAction: "absent"}

	tmp, err := os.MkdirTemp("", "pw-restore-")
	if err != nil {
		return nil, fmt.Errorf("backup: temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if err := extractArchive(archivePath, tmp); err != nil {
		return nil, err
	}

	// 1. 数据库。
	if m.Contains.DB {
		switch {
		case m.DBDriver == "mysql" && opt.Driver == "mysql":
			safety, err := safetyBackupMySQL(opt, tmp)
			if err != nil {
				return nil, err
			}
			rep.DBSafetyPath = safety
			if err := replayMySQLDump(opt.MySQLDSN, filepath.Join(tmp, dbMySQLPath)); err != nil {
				return nil, err
			}
			rep.AppliedDB = true
		case m.DBDriver == "sqlite" && opt.Driver == "sqlite":
			safety, err := safetyBackupSQLite(opt)
			if err != nil {
				return nil, err
			}
			rep.DBSafetyPath = safety
			// WAL/SHM 属于旧库,必须一并清掉,否则新快照打开即被旧日志污染。
			_ = os.Remove(opt.DBPath + "-wal")
			_ = os.Remove(opt.DBPath + "-shm")
			if err := copyFile(filepath.Join(tmp, dbSQLitePath), opt.DBPath, 0o600); err != nil {
				return nil, fmt.Errorf("backup: restore sqlite: %w", err)
			}
			rep.AppliedDB = true
		default:
			return nil, fmt.Errorf("%w: 备份来自 %s 模式,当前平台为 %s 模式,数据库不可跨驱动恢复", ErrBadArchive, m.DBDriver, opt.Driver)
		}
	} else {
		rep.Warnings = append(rep.Warnings, "归档不含数据库快照")
	}

	// 2. acme/ 与 artifacts/(合并覆盖:同名覆盖,其余保留)。
	if m.Contains.Acme {
		if err := mergeDir(filepath.Join(tmp, acmeDir), filepath.Join(opt.DataDir, "acme")); err != nil {
			rep.Warnings = append(rep.Warnings, "acme 目录恢复失败:"+err.Error())
		} else {
			rep.AppliedAcme = true
		}
	}
	if m.Contains.Artifacts {
		if err := mergeDir(filepath.Join(tmp, artifactsDir), filepath.Join(opt.DataDir, "artifacts")); err != nil {
			rep.Warnings = append(rep.Warnings, "artifacts 目录恢复失败:"+err.Error())
		} else {
			rep.AppliedArtifacts = true
		}
	}

	// 3. master key。
	if m.Contains.MasterKeySealed {
		switch {
		case opt.Passphrase == "":
			rep.MasterKeyAction = "present"
			rep.Warnings = append(rep.Warnings,
				"归档携带口令密封的 master key,但未提供口令,未恢复;重试需带口令(或手动配置 PIPEWRIGHT_MASTER_KEY 后再恢复一次)")
		default:
			raw, rerr := os.ReadFile(filepath.Join(tmp, masterKeyPath))
			if rerr != nil {
				return nil, fmt.Errorf("backup: read master.key: %w", rerr)
			}
			key, kerr := openMasterKeyWithPassphrase(raw, opt.Passphrase)
			if kerr != nil {
				return nil, fmt.Errorf("backup: %w(口令错误?)", kerr)
			}
			outPath := filepath.Join(opt.DataDir, "master.key.txt")
			if werr := os.WriteFile(outPath, key, 0o600); werr != nil {
				return nil, fmt.Errorf("backup: write master.key.txt: %w", werr)
			}
			rep.MasterKeyAction = "written"
			rep.MasterKeyFilePath = outPath
			rep.Warnings = append(rep.Warnings,
				"master key 已写入 master.key.txt;请将其配置为 PIPEWRIGHT_MASTER_KEY(_FILE)后重启,并妥善删除该文件")
		}
	}
	return rep, nil
}

// extractArchive 把归档解到 destDir(逐条 sha256 校验,拒绝路径穿越)。
func extractArchive(archivePath, destDir string) error {
	m, err := readManifest(archivePath)
	if err != nil {
		return err
	}
	registered := map[string]bool{}
	for _, e := range m.Files {
		registered[e.Name] = true
	}
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%w: tar: %v", ErrBadArchive, err)
		}
		if hdr.Typeflag != tar.TypeReg || !registered[hdr.Name] {
			// manifest.json 与目录条目跳过;未登记文件在校验阶段已拒绝,双保险丢弃。
			continue
		}
		clean := filepath.FromSlash(hdr.Name)
		if filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("%w: unsafe path %q", ErrBadArchive, hdr.Name)
		}
		target := filepath.Join(destDir, clean)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		h := sha256.New()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		n, cerr := io.Copy(io.MultiWriter(out, h), tr)
		_ = out.Close()
		if cerr != nil {
			return cerr
		}
		var want FileEntry
		for _, e := range m.Files {
			if e.Name == hdr.Name {
				want = e
				break
			}
		}
		if n != want.Size || hex.EncodeToString(h.Sum(nil)) != want.Sha256 {
			return fmt.Errorf("%w: checksum mismatch on extract %q", ErrBadArchive, hdr.Name)
		}
	}
}

// safetyBackupSQLite 把现库移动到 restore-safety/(保留最近 1 代;WAL/SHM 一并带走)。
func safetyBackupSQLite(opt RestoreOptions) (string, error) {
	safetyDir := filepath.Join(opt.DataDir, "restore-safety")
	if err := os.MkdirAll(safetyDir, 0o700); err != nil {
		return "", err
	}
	if _, err := os.Stat(opt.DBPath); errors.Is(err, os.ErrNotExist) {
		return "", nil // 现库不存在(全新安装恢复)→ 无需安全备份
	}
	// 清旧代(只保留最近一次恢复的安全备份)。
	entries, _ := os.ReadDir(safetyDir)
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(safetyDir, e.Name()))
	}
	dst := filepath.Join(safetyDir, "db-"+time.Now().UTC().Format("20060102-150405")+".sqlite")
	if err := os.Rename(opt.DBPath, dst); err != nil {
		return "", fmt.Errorf("backup: move current db: %w", err)
	}
	_ = os.Remove(opt.DBPath + "-wal")
	_ = os.Remove(opt.DBPath + "-shm")
	return dst, nil
}

// safetyBackupMySQL 用纯 Go dumper 把现库 dump 到 restore-safety/(CLI/启动期
// 尚无服务连接,这里按 DSN 自开短连接)。
func safetyBackupMySQL(opt RestoreOptions, tmpDir string) (string, error) {
	if strings.TrimSpace(opt.MySQLDSN) == "" {
		return "", fmt.Errorf("backup: mysql 模式恢复需要 MySQLDSN")
	}
	db, err := sql.Open("mysql", opt.MySQLDSN)
	if err != nil {
		return "", err
	}
	defer func() { _ = db.Close() }()
	if err := db.Ping(); err != nil {
		return "", fmt.Errorf("backup: ping mysql: %w", err)
	}
	safetyDir := filepath.Join(opt.DataDir, "restore-safety")
	if err := os.MkdirAll(safetyDir, 0o700); err != nil {
		return "", err
	}
	entries, _ := os.ReadDir(safetyDir)
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(safetyDir, e.Name()))
	}
	dst := filepath.Join(safetyDir, "db-"+time.Now().UTC().Format("20060102-150405")+".sql")
	f, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	d := &mysqlDumper{db: db, w: f}
	err = d.dump(context.Background())
	_ = f.Close()
	if err != nil {
		return "", fmt.Errorf("backup: safety dump: %w", err)
	}
	return dst, nil
}

// copyFile 复制文件内容并指定权限。
func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	_, err = io.Copy(out, in)
	return err
}

// mergeDir 把 src 目录合并进 dst(同名覆盖,其余保留;src 缺席 = 无操作)。
func mergeDir(src, dst string) error {
	if _, err := os.Stat(src); err != nil {
		return nil
	}
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, serr := d.Info()
		if serr != nil || !info.Mode().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return copyFile(p, target, info.Mode().Perm())
	})
}

// ── master key 的口令密封(argon2id 派生 + secretbox)─────────────────────────
//
// 格式:"pw1" || salt(16) || nonce(24) || box。派生参数与平台口令哈希同族
//(argon2id, t=1, m=64MB, p=4);口令错误在 open 时经 secretbox 认证失败暴露。

const (
	mkSaltLen   = 16
	mkNonceLen  = 24
	mkPrefix    = "pw1"
	argonTime   = 1
	argonMemory = 64 * 1024 // KiB
	argonThreads = 4
)

// sealMasterKeyWithPassphrase 读取当前 env 的 master key 并用口令密封。
func sealMasterKeyWithPassphrase(passphrase string) ([]byte, error) {
	key, err := config.LoadMasterKey()
	if err != nil {
		return nil, fmt.Errorf("backup: load master key: %w", err)
	}
	var salt [mkSaltLen]byte
	var nonce [mkNonceLen]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return nil, err
	}
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	dk := deriveKey(passphrase, salt[:])
	out := secretbox.Seal(nil, key[:], &nonce, &dk)
	buf := make([]byte, 0, len(mkPrefix)+mkSaltLen+mkNonceLen+len(out))
	buf = append(buf, mkPrefix...)
	buf = append(buf, salt[:]...)
	buf = append(buf, nonce[:]...)
	return append(buf, out...), nil
}

// openMasterKeyWithPassphrase 解开口令密封的 master key。
func openMasterKeyWithPassphrase(sealed []byte, passphrase string) ([]byte, error) {
	if len(sealed) < len(mkPrefix)+mkSaltLen+mkNonceLen+secretbox.Overhead || string(sealed[:len(mkPrefix)]) != mkPrefix {
		return nil, ErrBadArchive
	}
	off := len(mkPrefix)
	salt := sealed[off : off+mkSaltLen]
	off += mkSaltLen
	var nonce [mkNonceLen]byte
	copy(nonce[:], sealed[off:off+mkNonceLen])
	off += mkNonceLen
	dk := deriveKey(passphrase, salt)
	plain, ok := secretbox.Open(nil, sealed[off:], &nonce, &dk)
	if !ok {
		return nil, errors.New("backup: master key unseal failed")
	}
	return plain, nil
}

func deriveKey(passphrase string, salt []byte) [32]byte {
	var k [32]byte
	copy(k[:], argon2.IDKey([]byte(passphrase), salt, argonTime, argonMemory, argonThreads, 32))
	return k
}

// ── UI 在线恢复的 pending 标记 ────────────────────────────────────────────────
//
// UI 流程:上传归档 → 校验 → 暂存 restore-staging/ → 写 pending-restore.json →
// 空闲后自重启;启动期(main 打开存储之前)读到标记即应用并清除。口令不落明文:
// 用当前 master key secretbox 密封进标记(无 master key 且需要口令时 UI 侧提前拒绝)。

const (
	pendingMarkerName = "pending-restore.json"
	stagingDirName    = "restore-staging"
)

// PendingRestore 是延迟恢复标记。
type PendingRestore struct {
	Archive   string `json:"archive"`             // 相对数据目录的归档路径
	PassSealed []byte `json:"passSealed,omitempty"` // 口令密文(当前 master key 密封)
	CreatedAt string `json:"createdAt"`
}

// StagingDir 返回暂存目录路径。
func StagingDir(dataDir string) string { return filepath.Join(dataDir, stagingDirName) }

// WritePendingRestore 写标记(暂存归档已就位)。passphrase 非空时必须能取到当前
// master key 做密封;取不到则报错(调用方 UI 提前拦)。
func WritePendingRestore(dataDir, archiveRel, passphrase string) error {
	p := PendingRestore{
		Archive:   filepath.ToSlash(archiveRel),
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if strings.TrimSpace(passphrase) != "" {
		key, err := config.LoadMasterKey()
		if err != nil {
			return fmt.Errorf("backup: 携带口令的在线恢复需要平台已配置 master key: %w", err)
		}
		var nonce [24]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return err
		}
		p.PassSealed = secretbox.Seal(nil, []byte(passphrase), &nonce, key)
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dataDir, pendingMarkerName), raw, 0o600)
}

// ReadPendingRestore 读标记;不存在返回 (nil, nil)。
func ReadPendingRestore(dataDir string) (*PendingRestore, error) {
	raw, err := os.ReadFile(filepath.Join(dataDir, pendingMarkerName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p PendingRestore
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("backup: pending restore marker broken: %w", err)
	}
	return &p, nil
}

// ResolvePassphrase 用当前 master key 解开标记里的口令密文。
func (p *PendingRestore) ResolvePassphrase() (string, error) {
	if len(p.PassSealed) == 0 {
		return "", nil
	}
	key, err := config.LoadMasterKey()
	if err != nil {
		return "", fmt.Errorf("backup: 解开口令需要原 master key(环境未配置): %w", err)
	}
	if len(p.PassSealed) < 24 {
		return "", ErrBadArchive
	}
	var nonce [24]byte
	copy(nonce[:], p.PassSealed[:24])
	plain, ok := secretbox.Open(nil, p.PassSealed[24:], &nonce, key)
	if !ok {
		return "", errors.New("backup: 口令密文解开失败(master key 已变化?)")
	}
	return string(plain), nil
}

// ClearPendingRestore 清标记与暂存目录(应用成功或放弃时调用)。
func ClearPendingRestore(dataDir string) error {
	_ = os.RemoveAll(StagingDir(dataDir))
	return os.Remove(filepath.Join(dataDir, pendingMarkerName))
}

// ApplyPendingRestore 启动期入口:有标记则应用并清除,返回报告(无标记返回 nil)。
func ApplyPendingRestore(opt RestoreOptions) (*RestoreReport, error) {
	p, err := ReadPendingRestore(opt.DataDir)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, nil
	}
	pass, err := p.ResolvePassphrase()
	if err != nil {
		_ = ClearPendingRestore(opt.DataDir)
		return nil, fmt.Errorf("backup: 待恢复标记无法解开口令,已放弃本次在线恢复: %w", err)
	}
	archive := p.Archive
	if !filepath.IsAbs(archive) {
		archive = filepath.Join(opt.DataDir, filepath.FromSlash(archive))
	}
	rep, err := ApplyRestore(archive, RestoreOptions{
		Driver: opt.Driver, DataDir: opt.DataDir, DBPath: opt.DBPath,
		MySQLDSN: opt.MySQLDSN, Passphrase: pass,
	})
	_ = ClearPendingRestore(opt.DataDir)
	return rep, err
}
