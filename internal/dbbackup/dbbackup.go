// Package dbbackup 实现已登记数据库实例的备份恢复——功能长在 /databases 与
// /redis 两个现有页面(备份面板),不建任何新管理面。实例来源即 dbadmin/redisadmin
// 的登记(应用商店部署自动登记,手工登记外部实例亦可)。
//
// 备份路径按引擎与部署位置自动选择(用户只见"能/不能 + 原因"):
//   - MySQL:始终走数据库协议纯 Go dump(internal/backup.DumpMySQLAll,全库全保真);
//   - PostgreSQL:实例在纳管主机上(server_id+stack_name,应用商店部署)→ SSH+docker
//     exec pg_dumpall(全保真);外部实例(RDS 等)→ 协议子集 dump(表/列/默认值/
//     约束/索引/序列/数据/视图,不含函数/触发器/扩展,dump 头与 UI 均注明);
//   - Redis:只能 SSH+docker exec(容器内 redis-cli 导出 RDB);外部实例诚实不支持。
//
// 恢复前一律强制 pre_restore 安全备份;实例只读(readonly)时拒绝恢复(与
// dbadmin.Import 同纪律)。密码经 dbadmin/redisadmin 的受控出口在服务端进程内
// 即用即弃,绝不进响应/日志/审计;错误信息绝不携带执行的命令(argv 含密码)。
package dbbackup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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

	"github.com/huangchengsir/pipewright/internal/composeops"
	"github.com/huangchengsir/pipewright/internal/dbadmin"
	"github.com/huangchengsir/pipewright/internal/redisadmin"
	"github.com/huangchengsir/pipewright/internal/target"
)

// 实例类别(= 备份记录的 instance_kind):mysql/postgres 对应 db_instances,
// redis 对应 redis_instances。
const (
	KindMySQL    = "mysql"
	KindPostgres = "postgres"
	KindRedis    = "redis"
)

// 领域错误。
var (
	ErrInvalidInput = errors.New("dbbackup: invalid input")
	ErrNotFound     = errors.New("dbbackup: record not found")
	ErrUnsupported  = errors.New("dbbackup: backup unsupported for this instance")
	ErrBusy         = errors.New("dbbackup: another backup/restore is running on this instance")
	ErrReadonly     = errors.New("dbbackup: instance is read-only, unlock before restore")
)

// Config 是实例级备份计划(存于两张实例表的新增列)。
type Config struct {
	IntervalHours        int
	Retention            int
	LastBackupAt         string
	AutoUploadProviderID string
}

// Capability 告知前端这个实例能不能备份/恢复、走哪条路、有什么限制。
type Capability struct {
	Mode        string `json:"mode"` // protocol | ssh | none
	Backupable  bool   `json:"backupable"`
	Restoreable bool   `json:"restoreable"`
	Note        string `json:"note"` // 人话限制说明(可空)
}

// Record 是一次实例备份的运行记录。
type Record struct {
	ID           string
	InstanceKind string
	InstanceID   string
	FileName     string
	SizeBytes    int64
	Sha256       string
	Kind         string // manual|scheduled|pre_restore
	Status       string // running|completed|failed
	Error        string
	ObjectKey    string
	CreatedAt    string
	FinishedAt   string
}

// instanceRef 是从 dbadmin/redisadmin 取到的实例备份相关字段。
type instanceRef struct {
	ID        string
	Name      string
	Kind      string
	Host      string
	Port      int
	ServerID  string
	StackName string
	ReadOnly  bool
}

// Options 装配参数。
type Options struct {
	// DataDir 控制机数据目录(备份落 <DataDir>/db-backups/<实例ID>/)。
	DataDir string
	// Uploader 可选对象存储上传(objectstore.Service 满足;nil = 禁用上云)。
	Uploader Uploader
	Logf     func(format string, args ...any)
}

// Uploader 与 backup.Uploader/objectstore.Service.Upload 同形。
type Uploader interface {
	Upload(ctx context.Context, providerID, objectKey string, body io.Reader) error
}

// Service 是实例备份领域服务。
type Service struct {
	db      *sql.DB
	dba     dbadmin.Service
	redisa  redisadmin.Service
	targets target.Service
	opt     Options

	mu       sync.Mutex
	inflight map[string]bool // "kind/instanceID" → 有备份或恢复进行中
}

// New 构造服务。
func New(db *sql.DB, dba dbadmin.Service, redisa redisadmin.Service, targets target.Service, opt Options) *Service {
	if opt.Logf == nil {
		opt.Logf = log.Printf
	}
	return &Service{db: db, dba: dba, redisa: redisa, targets: targets, opt: opt, inflight: map[string]bool{}}
}

func (s *Service) logf(format string, args ...any) { s.opt.Logf("[dbbackup] "+format, args...) }

// BackupsRoot 返回备份落盘根目录。
func (s *Service) BackupsRoot() string { return filepath.Join(s.opt.DataDir, "db-backups") }

// instanceTable 返回类别对应的实例表名。
func instanceTable(kind string) (string, error) {
	switch kind {
	case KindMySQL, KindPostgres:
		return "db_instances", nil
	case KindRedis:
		return "redis_instances", nil
	default:
		return "", fmt.Errorf("%w: 未知实例类别 %q", ErrInvalidInput, kind)
	}
}

// ref 取实例的备份相关字段。
func (s *Service) ref(ctx context.Context, kind, instanceID string) (*instanceRef, error) {
	switch kind {
	case KindMySQL, KindPostgres:
		inst, err := s.dba.Get(ctx, instanceID)
		if err != nil {
			return nil, fmt.Errorf("%w: 实例不存在", ErrNotFound)
		}
		return &instanceRef{ID: inst.ID, Name: inst.Name, Kind: inst.Engine, Host: inst.Host,
			Port: inst.Port, ServerID: inst.ServerID, StackName: inst.StackName, ReadOnly: inst.ReadOnly}, nil
	case KindRedis:
		inst, err := s.redisa.Get(ctx, instanceID)
		if err != nil {
			return nil, fmt.Errorf("%w: 实例不存在", ErrNotFound)
		}
		return &instanceRef{ID: inst.ID, Name: inst.Name, Kind: KindRedis, Host: inst.Host,
			Port: inst.Port, ServerID: inst.ServerID, StackName: inst.StackName, ReadOnly: inst.ReadOnly}, nil
	default:
		return nil, fmt.Errorf("%w: 未知实例类别 %q", ErrInvalidInput, kind)
	}
}

// managed 报告实例是否部署在纳管主机上(应用商店部署 → server_id+stack_name)。
func (r *instanceRef) managed() bool {
	return r.ServerID != "" && r.StackName != ""
}

// Capability 判定实例的备份能力。
func (s *Service) Capability(ctx context.Context, kind, instanceID string) (Capability, error) {
	ref, err := s.ref(ctx, kind, instanceID)
	if err != nil {
		return Capability{}, err
	}
	switch ref.Kind {
	case KindMySQL:
		return Capability{Mode: "protocol", Backupable: true, Restoreable: true}, nil
	case KindPostgres:
		if ref.managed() {
			return Capability{Mode: "ssh", Backupable: true, Restoreable: true}, nil
		}
		return Capability{
			Mode: "protocol", Backupable: true, Restoreable: true,
			Note: "外部 PostgreSQL:协议备份为子集保真(不含函数/触发器/扩展);全保真需实例部署在纳管主机(应用商店部署)",
		}, nil
	case KindRedis:
		if ref.managed() {
			return Capability{Mode: "ssh", Backupable: true, Restoreable: true}, nil
		}
		return Capability{
			Mode: "none", Backupable: false, Restoreable: false,
			Note: "外部 Redis 无法远程导出 RDB;备份需实例由应用商店部署(平台可 SSH 到宿主机)",
		}, nil
	default:
		return Capability{}, fmt.Errorf("%w: 未知引擎 %q", ErrInvalidInput, ref.Kind)
	}
}

// GetConfig 读实例备份计划。
func (s *Service) GetConfig(ctx context.Context, kind, instanceID string) (Config, error) {
	table, err := instanceTable(kind)
	if err != nil {
		return Config{}, err
	}
	var c Config
	err = s.db.QueryRowContext(ctx,
		`SELECT backup_interval_hours, backup_retention, last_backup_at, auto_upload_provider_id FROM `+table+` WHERE id = ?`, instanceID,
	).Scan(&c.IntervalHours, &c.Retention, &c.LastBackupAt, &c.AutoUploadProviderID)
	if errors.Is(err, sql.ErrNoRows) {
		return Config{}, fmt.Errorf("%w: 实例不存在", ErrNotFound)
	}
	if err != nil {
		return Config{}, fmt.Errorf("dbbackup: get config: %w", err)
	}
	return c, nil
}

// SetConfig 写实例备份计划(校验区间;provider 白名单交给 httpapi 层,此处仅去空白)。
func (s *Service) SetConfig(ctx context.Context, kind, instanceID string, c Config) error {
	table, err := instanceTable(kind)
	if err != nil {
		return err
	}
	if c.IntervalHours < 0 || c.IntervalHours > 24*30 {
		return fmt.Errorf("%w: 备份间隔须在 0..720 小时", ErrInvalidInput)
	}
	if c.Retention < 1 || c.Retention > 365 {
		c.Retention = 7
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE `+table+` SET backup_interval_hours = ?, backup_retention = ?, auto_upload_provider_id = ? WHERE id = ?`,
		c.IntervalHours, c.Retention, strings.TrimSpace(c.AutoUploadProviderID), instanceID)
	if err != nil {
		return fmt.Errorf("dbbackup: set config: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: 实例不存在", ErrNotFound)
	}
	return nil
}

// TouchLastRun 推进 last_backup_at(定时器触发即写,防重复触发风暴)。
func (s *Service) TouchLastRun(ctx context.Context, kind, instanceID string) error {
	table, err := instanceTable(kind)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE `+table+` SET last_backup_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), instanceID)
	return err
}

// acquire 标记实例忙(备份/恢复互斥);已在进行 → ErrBusy。
func (s *Service) acquire(kind, instanceID string) (release func(), ok bool) {
	key := kind + "/" + instanceID
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight[key] {
		return nil, false
	}
	s.inflight[key] = true
	return func() {
		s.mu.Lock()
		delete(s.inflight, key)
		s.mu.Unlock()
	}, true
}

// RunBackup 登记运行记录并异步执行备份,立即返回记录(前端轮询状态)。
// kindOfRun: manual|scheduled|pre_restore。
func (s *Service) RunBackup(ctx context.Context, kind, instanceID, kindOfRun string) (*Record, error) {
	ref, err := s.ref(ctx, kind, instanceID)
	if err != nil {
		return nil, err
	}
	capability, err := s.Capability(ctx, kind, instanceID)
	if err != nil {
		return nil, err
	}
	if !capability.Backupable {
		return nil, fmt.Errorf("%w:%s", ErrUnsupported, capability.Note)
	}
	release, ok := s.acquire(kind, instanceID)
	if !ok {
		return nil, ErrBusy
	}

	ext := "sql"
	if ref.Kind == KindRedis {
		ext = "rdb"
	}
	rec := &Record{
		ID: "dbb-" + uuid.NewString(), InstanceKind: ref.Kind, InstanceID: ref.ID,
		Kind: kindOfRun, Status: "running", CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	rec.FileName = fmt.Sprintf("%s-%s.%s", sanitizeFileName(ref.Name), strings.ReplaceAll(time.Now().Format("20060102-150405"), ".", ""), ext)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO db_backup_records (id, instance_kind, instance_id, file_name, size_bytes, sha256, kind, status, error, object_key, created_at)
		 VALUES (?, ?, ?, ?, 0, '', ?, 'running', '', '', ?)`,
		rec.ID, rec.InstanceKind, rec.InstanceID, rec.FileName, rec.Kind, rec.CreatedAt); err != nil {
		release()
		return nil, fmt.Errorf("dbbackup: 登记备份失败: %w", err)
	}

	cfg, _ := s.GetConfig(ctx, kind, instanceID)
	// 备份与请求生命周期解耦:页面关闭不中断 dump。
	go s.runBackup(context.Background(), ref, capability, rec, cfg, release)
	return rec, nil
}

// sanitizeFileName 把实例名压成文件名安全形式(实例名本身已受限,此处防御)。
func sanitizeFileName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		out = "instance"
	}
	return out
}

// runBackup 执行备份主体:dump → 落盘 → 终态 → 自动上云(配置了 provider)→ 保留裁剪。
func (s *Service) runBackup(ctx context.Context, ref *instanceRef, capability Capability, rec *Record, cfg Config, release func()) {
	defer release()

	err := func() error {
		dir := filepath.Join(s.BackupsRoot(), rec.InstanceID)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		path := filepath.Join(dir, rec.FileName)
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		var dumpErr error
		switch {
		case ref.Kind == KindMySQL:
			dumpErr = s.dumpMySQLProtocol(ctx, ref, f)
		case ref.Kind == KindPostgres && capability.Mode == "ssh":
			dumpErr = s.dumpPostgresSSH(ctx, ref, f)
		case ref.Kind == KindPostgres:
			dumpErr = s.dumpPostgresProtocol(ctx, ref, f)
		case ref.Kind == KindRedis:
			dumpErr = s.dumpRedisSSH(ctx, ref, f)
		default:
			dumpErr = fmt.Errorf("%w: %s", ErrUnsupported, ref.Kind)
		}
		_ = f.Close()
		if dumpErr != nil {
			_ = os.Remove(path)
			return dumpErr
		}
		sum, size, err := fileSha256(path)
		if err != nil {
			return err
		}
		if size == 0 {
			_ = os.Remove(path)
			return fmt.Errorf("备份数据为空(dump 未产出)")
		}
		rec.Sha256, rec.SizeBytes = sum, size
		return nil
	}()

	finished := time.Now().UTC().Format(time.RFC3339)
	if err != nil {
		rec.Status, rec.Error, rec.FinishedAt = "failed", err.Error(), finished
		s.logf("备份失败(%s):%v", ref.Name, err)
	} else {
		rec.Status, rec.FinishedAt = "completed", finished
		s.logf("备份完成:%s → %s(%d 字节)", ref.Name, rec.FileName, rec.SizeBytes)
	}
	s.finishRecord(rec)

	if rec.Status == "completed" && cfg.AutoUploadProviderID != "" && s.opt.Uploader != nil {
		s.uploadRecord(context.Background(), rec, ref.Name, cfg.AutoUploadProviderID)
	}
	if rec.Status == "completed" && cfg.Retention > 0 {
		if err := s.applyRetention(context.Background(), rec.InstanceKind, rec.InstanceID, cfg.Retention); err != nil {
			s.logf("保留裁剪失败(best-effort,%s):%v", ref.Name, err)
		}
	}
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

// finishRecord 写回终态。
func (s *Service) finishRecord(rec *Record) {
	if _, err := s.db.Exec(
		`UPDATE db_backup_records SET size_bytes = ?, sha256 = ?, status = ?, error = ?, object_key = ?, finished_at = ? WHERE id = ?`,
		rec.SizeBytes, rec.Sha256, rec.Status, rec.Error, rec.ObjectKey, rec.FinishedAt, rec.ID); err != nil {
		s.logf("更新备份记录失败(%s):%v", rec.ID, err)
	}
}

// uploadRecord 把备份文件上传到对象存储(键 db-backups/<实例名>/<文件>)。
func (s *Service) uploadRecord(ctx context.Context, rec *Record, instanceName, providerID string) {
	f, err := os.Open(filepath.Join(s.BackupsRoot(), rec.InstanceID, rec.FileName))
	if err != nil {
		s.logf("备份上传对象存储失败(打开文件):%v", err)
		return
	}
	defer func() { _ = f.Close() }()
	key := "db-backups/" + sanitizeFileName(instanceName) + "/" + rec.FileName
	if err := s.opt.Uploader.Upload(ctx, providerID, key, f); err != nil {
		s.logf("备份上传对象存储失败(best-effort):%v", err)
		return
	}
	rec.ObjectKey = key
	s.finishRecord(rec)
	s.logf("备份已上传对象存储:%s", key)
}

// UploadToObjectStore 手动上传一条备份(HTTP 入口)。
func (s *Service) UploadToObjectStore(ctx context.Context, recordID, providerID string) error {
	rec, err := s.Get(ctx, recordID)
	if err != nil {
		return err
	}
	if rec.Status != "completed" {
		return fmt.Errorf("%w: 只有完成的备份才能上传", ErrInvalidInput)
	}
	if s.opt.Uploader == nil {
		return fmt.Errorf("dbbackup: 对象存储服务未装配")
	}
	ref, err := s.ref(ctx, rec.InstanceKind, rec.InstanceID)
	if err != nil {
		return err
	}
	s.uploadRecord(ctx, rec, ref.Name, providerID)
	return nil
}

// applyRetention 按保留份数裁剪最老的已完成备份(连同磁盘文件)。
func (s *Service) applyRetention(ctx context.Context, kind, instanceID string, retention int) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, file_name FROM db_backup_records WHERE instance_kind = ? AND instance_id = ? AND status = 'completed'
		 ORDER BY created_at DESC, id DESC`, kind, instanceID)
	if err != nil {
		return err
	}
	type idFile struct{ id, name string }
	var all []idFile
	for rows.Next() {
		var e idFile
		if err := rows.Scan(&e.id, &e.name); err != nil {
			_ = rows.Close()
			return err
		}
		all = append(all, e)
	}
	_ = rows.Close()
	if len(all) <= retention {
		return nil
	}
	for _, e := range all[retention:] {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM db_backup_records WHERE id = ?`, e.id); err != nil {
			return err
		}
		if e.name != "" {
			_ = os.Remove(filepath.Join(s.BackupsRoot(), instanceID, e.name))
		}
	}
	return nil
}

// List 列实例的备份(新→老)。
func (s *Service) List(ctx context.Context, kind, instanceID string) ([]Record, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, instance_kind, instance_id, file_name, size_bytes, sha256, kind, status, error, object_key, created_at, finished_at
		 FROM db_backup_records WHERE instance_kind = ? AND instance_id = ? ORDER BY created_at DESC, id DESC`, kind, instanceID)
	if err != nil {
		return nil, fmt.Errorf("dbbackup: list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Record
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.ID, &r.InstanceKind, &r.InstanceID, &r.FileName, &r.SizeBytes, &r.Sha256,
			&r.Kind, &r.Status, &r.Error, &r.ObjectKey, &r.CreatedAt, &r.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Get 取单条备份记录。
func (s *Service) Get(ctx context.Context, id string) (*Record, error) {
	var r Record
	err := s.db.QueryRowContext(ctx,
		`SELECT id, instance_kind, instance_id, file_name, size_bytes, sha256, kind, status, error, object_key, created_at, finished_at
		 FROM db_backup_records WHERE id = ?`, id,
	).Scan(&r.ID, &r.InstanceKind, &r.InstanceID, &r.FileName, &r.SizeBytes, &r.Sha256,
		&r.Kind, &r.Status, &r.Error, &r.ObjectKey, &r.CreatedAt, &r.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// BackupPath 返回备份文件磁盘路径。
func (s *Service) BackupPath(rec *Record) string {
	return filepath.Join(s.BackupsRoot(), rec.InstanceID, rec.FileName)
}

// Delete 删除备份记录与磁盘文件(幂等)。
func (s *Service) Delete(ctx context.Context, id string) error {
	rec, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM db_backup_records WHERE id = ?`, id); err != nil {
		return err
	}
	_ = os.Remove(s.BackupPath(rec))
	return nil
}

// DeleteForInstance 实例删除时的级联清理(记录 + 磁盘文件;httpapi 删除实例时调用)。
func (s *Service) DeleteForInstance(ctx context.Context, kind, instanceID string) error {
	if _, err := instanceTable(kind); err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, file_name FROM db_backup_records WHERE instance_kind = ? AND instance_id = ?`, kind, instanceID)
	if err != nil {
		return err
	}
	type idFile struct{ id, name string }
	var all []idFile
	for rows.Next() {
		var e idFile
		if err := rows.Scan(&e.id, &e.name); err != nil {
			_ = rows.Close()
			return err
		}
		all = append(all, e)
	}
	_ = rows.Close()
	for _, e := range all {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM db_backup_records WHERE id = ?`, e.id); err == nil && e.name != "" {
			_ = os.Remove(filepath.Join(s.BackupsRoot(), instanceID, e.name))
		}
	}
	_ = os.RemoveAll(filepath.Join(s.BackupsRoot(), instanceID))
	return nil
}

// containerID 定位栈的首个容器(SSH 路径用)。
func (s *Service) containerID(ctx context.Context, ref *instanceRef) (string, error) {
	ids, err := composeops.ProjectContainerIDs(ctx, s.targets, ref.ServerID, ref.StackName)
	if err != nil {
		return "", fmt.Errorf("取容器失败: %w", err)
	}
	if len(ids) == 0 {
		return "", fmt.Errorf("栈 %s 无容器(实例可能已停止;SSH 路径需要容器在运行)", ref.StackName)
	}
	return ids[0], nil
}

// trimOut 截断输出用于错误信息(只含 stderr,不含命令)。
func trimOut(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) > max {
		return s[:max] + "…"
	}
	if s == "" {
		return "(无输出)"
	}
	return s
}
