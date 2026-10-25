// backup.go:备份服务核心——配置读写、备份执行(SQLite 快照 / MySQL dump)、
// 记录列表/删除、对象存储上传、保留裁剪。
package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/huangchengsir/pipewright/internal/version"
)

// Config 是平台备份策略(单行 platform_backup_config)。
type Config struct {
	Enabled               bool
	IntervalHours         int
	RetentionCount        int
	IncludeArtifacts      bool
	IncludeMasterKey      bool
	ObjectstoreProviderID string
	LastRunAt             string
}

// Record 是一次备份的运行记录。
type Record struct {
	ID         string
	FileName   string
	SizeBytes  int64
	Sha256     string
	Kind       string // manual|scheduled
	Status     string // running|completed|failed
	Error      string
	ObjectKey  string
	DBDriver   string
	CreatedAt  string
	FinishedAt string
}

// CreateInput 触发一次备份的入参。
type CreateInput struct {
	Kind             string // manual|scheduled
	IncludeArtifacts *bool  // nil = 用配置值
	// Passphrase 为空且配置开启 include_master_key 时,本次备份不带 master key
	//(记录一条 warning 进 error 字段前缀,不失败)。
	Passphrase string
}

// Uploader 把备份包上传到对象存储(objectstore.Service 满足此接口;nil = 禁用)。
type Uploader interface {
	Upload(ctx context.Context, providerID, objectKey string, body io.Reader) error
}

// Options 装配参数。DataDir 为数据目录(DB 同级);DBPath 为 SQLite 库文件路径
// (仅 sqlite 模式使用);MySQLDSN 供恢复回放/安全备份时另开多语句连接。
type Options struct {
	Driver   string // sqlite|mysql
	DataDir  string
	DBPath   string
	MySQLDSN string
	Uploader Uploader
	Logf     func(format string, args ...any)
}

// Service 是备份领域服务。
type Service struct {
	db  *sql.DB
	opt Options

	mu       sync.Mutex
	inflight map[string]bool // 备份 id → 进行中(防重复触发同一 id)
}

// New 构造备份服务。
func New(db *sql.DB, opt Options) *Service {
	if opt.Driver == "" {
		opt.Driver = "sqlite"
	}
	if opt.Logf == nil {
		opt.Logf = log.Printf
	}
	return &Service{db: db, opt: opt, inflight: map[string]bool{}}
}

// BackupsDir 返回备份归档落盘目录(数据目录下 backups/)。
func (s *Service) BackupsDir() string { return filepath.Join(s.opt.DataDir, "backups") }

// DataDir 返回数据目录(恢复标记/暂存目录的锚点)。
func (s *Service) DataDir() string { return s.opt.DataDir }

func (s *Service) logf(format string, args ...any) {
	s.opt.Logf("[backup] "+format, args...)
}

// GetConfig 读取备份策略(缺行返回零值)。
func (s *Service) GetConfig(ctx context.Context) (Config, error) {
	var (
		enabled, interval, retention int
		includeArtifacts, includeKey int
		provider, lastRun            string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT enabled, interval_hours, retention_count, include_artifacts, include_master_key, objectstore_provider_id, last_run_at FROM platform_backup_config WHERE id = 1`,
	).Scan(&enabled, &interval, &retention, &includeArtifacts, &includeKey, &provider, &lastRun)
	if errors.Is(err, sql.ErrNoRows) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("backup: get config: %w", err)
	}
	return Config{
		Enabled:               enabled != 0,
		IntervalHours:         interval,
		RetentionCount:        retention,
		IncludeArtifacts:      includeArtifacts != 0,
		IncludeMasterKey:      includeKey != 0,
		ObjectstoreProviderID: provider,
		LastRunAt:             lastRun,
	}, nil
}

// SetConfig 持久化备份策略(负值归一;upsert 单行)。
func (s *Service) SetConfig(ctx context.Context, c Config) error {
	if c.IntervalHours < 0 {
		c.IntervalHours = 0
	}
	if c.RetentionCount < 0 {
		c.RetentionCount = 0
	}
	enabled, incArt, incKey := 0, 0, 0
	if c.Enabled {
		enabled = 1
	}
	if c.IncludeArtifacts {
		incArt = 1
	}
	if c.IncludeMasterKey {
		incKey = 1
	}
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx,
		`UPDATE platform_backup_config SET enabled = ?, interval_hours = ?, retention_count = ?, include_artifacts = ?, include_master_key = ?, objectstore_provider_id = ?, updated_at = ? WHERE id = 1`,
		enabled, c.IntervalHours, c.RetentionCount, incArt, incKey, strings.TrimSpace(c.ObjectstoreProviderID), now)
	if err != nil {
		return fmt.Errorf("backup: set config: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO platform_backup_config (id, enabled, interval_hours, retention_count, include_artifacts, include_master_key, objectstore_provider_id, updated_at) VALUES (1, ?, ?, ?, ?, ?, ?, ?)`,
			enabled, c.IntervalHours, c.RetentionCount, incArt, incKey, strings.TrimSpace(c.ObjectstoreProviderID), now); err != nil {
			return fmt.Errorf("backup: insert config: %w", err)
		}
	}
	return nil
}

// TouchLastRun 把 last_run_at 推进到 now(定时器触发即写,防重复触发风暴)。
func (s *Service) TouchLastRun(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE platform_backup_config SET last_run_at = ? WHERE id = 1`,
		time.Now().UTC().Format(time.RFC3339))
	return err
}

// CreateBackup 登记运行记录并异步执行备份,立即返回记录(前端轮询状态)。
// 同一时间仅允许一个备份在跑(全局互斥:快照/打包是 IO 密集,串行足够)。
func (s *Service) CreateBackup(ctx context.Context, in CreateInput) (*Record, error) {
	s.mu.Lock()
	if len(s.inflight) > 0 {
		s.mu.Unlock()
		return nil, ErrBackupRunning
	}
	rec := &Record{
		ID:        "bk-" + uuid.NewString(),
		Kind:      in.Kind,
		Status:    "running",
		DBDriver:  s.opt.Driver,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	rec.FileName = fmt.Sprintf("pw-backup-%s.pwb", strings.ReplaceAll(time.Now().Format("20060102-150405"), ".", ""))
	s.inflight[rec.ID] = true
	s.mu.Unlock()

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO platform_backups (id, file_name, size_bytes, sha256, kind, status, error, object_key, db_driver, created_at) VALUES (?, ?, 0, '', ?, 'running', '', '', ?, ?)`,
		rec.ID, rec.FileName, rec.Kind, rec.DBDriver, rec.CreatedAt); err != nil {
		s.mu.Lock()
		delete(s.inflight, rec.ID)
		s.mu.Unlock()
		return nil, fmt.Errorf("backup: insert record: %w", err)
	}

	cfg, _ := s.GetConfig(ctx)
	includeArtifacts := cfg.IncludeArtifacts
	if in.IncludeArtifacts != nil {
		includeArtifacts = *in.IncludeArtifacts
	}

	go s.runBackup(rec, includeArtifacts, cfg.IncludeMasterKey, in.Passphrase, cfg.ObjectstoreProviderID)
	return rec, nil
}

// ErrBackupRunning 表示已有备份进行中。
var ErrBackupRunning = errors.New("backup: another backup is running")

// runBackup 执行备份主体:快照 → 打包 → 落记录 → 保留裁剪 → 可选上云。
func (s *Service) runBackup(rec *Record, includeArtifacts, includeMasterKey bool, passphrase, uploadProviderID string) {
	defer func() {
		s.mu.Lock()
		delete(s.inflight, rec.ID)
		s.mu.Unlock()
	}()

	err := func() error {
		if err := os.MkdirAll(s.BackupsDir(), 0o755); err != nil {
			return fmt.Errorf("mkdir backups: %w", err)
		}
		tmpDir, err := os.MkdirTemp("", "pw-backup-")
		if err != nil {
			return fmt.Errorf("temp dir: %w", err)
		}
		defer func() { _ = os.RemoveAll(tmpDir) }()

		// 1. 数据库快照(SQLite VACUUM INTO / MySQL SQL dump)。
		dbFile := filepath.Join(tmpDir, "db.snapshot")
		if s.opt.Driver == "mysql" {
			f, err := os.Create(dbFile)
			if err != nil {
				return err
			}
			err = s.dumpMySQL(context.Background(), f)
			_ = f.Close()
			if err != nil {
				return err
			}
		} else {
			if err := s.snapshotSQLite(context.Background(), dbFile); err != nil {
				return err
			}
		}

		// 2. master key(可选,口令密封)。
		var masterSealed []byte
		if includeMasterKey {
			if strings.TrimSpace(passphrase) == "" {
				rec.Error = "warning: include_master_key 已开启但未提供口令,本次备份未携带 master key"
			} else {
				sealed, err := sealMasterKeyWithPassphrase(passphrase)
				if err != nil {
					return fmt.Errorf("seal master key: %w", err)
				}
				masterSealed = sealed
			}
		}

		// 3. 打包(写 .part 后原子 rename,避免半成品被下载)。
		partPath := filepath.Join(s.BackupsDir(), rec.FileName+".part")
		finalPath := filepath.Join(s.BackupsDir(), rec.FileName)
		b, err := newArchiveBuilder(partPath, version.String(), s.opt.Driver)
		if err != nil {
			return err
		}
		dbEntry := dbSQLitePath
		if s.opt.Driver == "mysql" {
			dbEntry = dbMySQLPath
		}
		if err := b.addFile(dbEntry, dbFile); err != nil {
			b.abort()
			_ = os.Remove(partPath)
			return err
		}
		contains := Contains{DB: true, Acme: true}
		if err := b.addDir(acmeDir, filepath.Join(s.opt.DataDir, "acme")); err != nil {
			b.abort()
			_ = os.Remove(partPath)
			return err
		}
		if includeArtifacts {
			if err := b.addDir(artifactsDir, filepath.Join(s.opt.DataDir, "artifacts")); err != nil {
				b.abort()
				_ = os.Remove(partPath)
				return err
			}
			contains.Artifacts = true
		}
		if masterSealed != nil {
			if err := b.addBytes(masterKeyPath, masterSealed); err != nil {
				b.abort()
				_ = os.Remove(partPath)
				return err
			}
			contains.MasterKeySealed = true
		}
		if err := b.Close(rec.CreatedAt, contains); err != nil {
			b.abort()
			_ = os.Remove(partPath)
			return err
		}
		if err := os.Rename(partPath, finalPath); err != nil {
			_ = os.Remove(partPath)
			return fmt.Errorf("finalize archive: %w", err)
		}

		sum, size, err := fileSha256(finalPath)
		if err != nil {
			return fmt.Errorf("hash archive: %w", err)
		}
		rec.Sha256, rec.SizeBytes = sum, size
		return nil
	}()

	finished := time.Now().UTC().Format(time.RFC3339)
	if err != nil {
		rec.Status, rec.FinishedAt = "failed", finished
		errMsg := err.Error()
		if rec.Error != "" {
			errMsg = rec.Error + "; " + errMsg
		}
		rec.Error = errMsg
		s.logf("备份失败(%s):%v", rec.ID, err)
	} else {
		rec.Status, rec.FinishedAt = "completed", finished
		if rec.Error != "" {
			rec.Error += ";"
		}
		s.logf("备份完成:%s(%d 字节)", rec.FileName, rec.SizeBytes)
	}
	s.finishRecord(rec)

	if rec.Status == "completed" && uploadProviderID != "" && s.opt.Uploader != nil {
		s.uploadToObjectStore(context.Background(), rec, uploadProviderID)
	}
	if rec.Status == "completed" {
		if err := s.applyRetention(context.Background()); err != nil {
			s.logf("保留裁剪失败(best-effort):%v", err)
		}
	}
}

// finishRecord 把终态写回运行记录。
func (s *Service) finishRecord(rec *Record) {
	_, err := s.db.Exec(
		`UPDATE platform_backups SET size_bytes = ?, sha256 = ?, status = ?, error = ?, object_key = ?, finished_at = ? WHERE id = ?`,
		rec.SizeBytes, rec.Sha256, rec.Status, rec.Error, rec.ObjectKey, rec.FinishedAt, rec.ID)
	if err != nil {
		s.logf("更新备份记录失败(%s):%v", rec.ID, err)
	}
}

// uploadToObjectStore 把归档上传到配置的对象存储(对象键 platform-backups/<file>)。
func (s *Service) uploadToObjectStore(ctx context.Context, rec *Record, providerID string) {
	f, err := os.Open(filepath.Join(s.BackupsDir(), rec.FileName))
	if err != nil {
		s.logf("备份上传对象存储失败(打开文件):%v", err)
		return
	}
	defer func() { _ = f.Close() }()
	key := "platform-backups/" + rec.FileName
	if err := s.opt.Uploader.Upload(ctx, providerID, key, f); err != nil {
		s.logf("备份上传对象存储失败(best-effort):%v", err)
		return
	}
	rec.ObjectKey = key
	s.finishRecord(rec)
	s.logf("备份已上传对象存储:%s", key)
}

// UploadToObjectStore 按备份 id 上传归档到指定对象存储配置(HTTP 手动触发入口;
// 定时备份的自动上传在 runBackup 内完成)。
func (s *Service) UploadToObjectStore(ctx context.Context, id, providerID string) error {
	rec, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if rec.Status != "completed" {
		return fmt.Errorf("backup: 只有完成的备份才能上传")
	}
	if s.opt.Uploader == nil {
		return fmt.Errorf("backup: 对象存储服务未装配")
	}
	f, err := os.Open(s.ArchivePath(rec))
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	key := "platform-backups/" + rec.FileName
	if err := s.opt.Uploader.Upload(ctx, providerID, key, f); err != nil {
		return err
	}
	rec.ObjectKey = key
	s.finishRecord(rec)
	return nil
}

// applyRetention 按配置份数裁剪最老的已完成备份(连同磁盘文件)。
func (s *Service) applyRetention(ctx context.Context) error {
	cfg, err := s.GetConfig(ctx)
	if err != nil {
		return err
	}
	if cfg.RetentionCount <= 0 {
		return nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, file_name FROM platform_backups WHERE status = 'completed' ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	type idFile struct{ id, name string }
	var all []idFile
	for rows.Next() {
		var e idFile
		if err := rows.Scan(&e.id, &e.name); err != nil {
			return err
		}
		all = append(all, e)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(all) <= cfg.RetentionCount {
		return nil
	}
	for _, e := range all[cfg.RetentionCount:] {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM platform_backups WHERE id = ?`, e.id); err != nil {
			return err
		}
		if e.name != "" {
			_ = os.Remove(filepath.Join(s.BackupsDir(), e.name))
		}
	}
	return nil
}

// List 返回全部备份记录(新→老)。
func (s *Service) List(ctx context.Context) ([]Record, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, file_name, size_bytes, sha256, kind, status, error, object_key, db_driver, created_at, finished_at FROM platform_backups ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("backup: list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Record
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.ID, &r.FileName, &r.SizeBytes, &r.Sha256, &r.Kind, &r.Status, &r.Error, &r.ObjectKey, &r.DBDriver, &r.CreatedAt, &r.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Get 返回单条记录。
func (s *Service) Get(ctx context.Context, id string) (*Record, error) {
	var r Record
	err := s.db.QueryRowContext(ctx,
		`SELECT id, file_name, size_bytes, sha256, kind, status, error, object_key, db_driver, created_at, finished_at FROM platform_backups WHERE id = ?`, id,
	).Scan(&r.ID, &r.FileName, &r.SizeBytes, &r.Sha256, &r.Kind, &r.Status, &r.Error, &r.ObjectKey, &r.DBDriver, &r.CreatedAt, &r.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("backup: get: %w", err)
	}
	return &r, nil
}

// ErrNotFound 表示备份记录不存在。
var ErrNotFound = errors.New("backup: record not found")

// ArchivePath 返回记录对应归档的磁盘路径。
func (s *Service) ArchivePath(rec *Record) string {
	return filepath.Join(s.BackupsDir(), rec.FileName)
}

// Delete 删除记录与磁盘归档(幂等)。
func (s *Service) Delete(ctx context.Context, id string) error {
	rec, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM platform_backups WHERE id = ?`, id); err != nil {
		return fmt.Errorf("backup: delete: %w", err)
	}
	_ = os.Remove(s.ArchivePath(rec))
	return nil
}

// snapshotSQLite 用 VACUUM INTO 产出一份数据库快照(在线一致;目标文件必须不存在)。
func (s *Service) snapshotSQLite(ctx context.Context, target string) error {
	if _, err := os.Stat(target); err == nil {
		_ = os.Remove(target)
	}
	// VACUUM INTO 的文件名走字面量(驱动对 DDL 内参数绑定支持不一),单引号转义防注入。
	quoted := "'" + strings.ReplaceAll(strings.ReplaceAll(target, "'", "''"), "\\", "\\\\") + "'"
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO "+quoted); err != nil {
		return fmt.Errorf("vacuum into: %w", err)
	}
	return nil
}
