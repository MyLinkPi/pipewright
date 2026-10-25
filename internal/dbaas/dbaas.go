// Package dbaas 实现数据库即服务:按需创建/管理 MySQL、PostgreSQL、Redis 实例
// (选服务器、版本、端口、口令、资源限额),统一生命周期(创建/启停/删除)、
// 备份恢复(引擎原生 dump,流式经 SSH 中转)与定时备份。
//
// 部署复用应用商店的受管 compose 链路(internal/composeops):栈名 db-<name>,
// 数据落具名卷;创建成功后联动登记进 dbadmin/redisadmin(在线管理页即可直接用)。
// 口令经 vault 密文落库;目标机侧 compose/环境与本包执行的维护命令中存在明文
// (与应用商店同安全等级——目标机本身就是口令持有者);错误信息绝不携带 argv。
package dbaas

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/huangchengsir/pipewright/internal/composeops"
	"github.com/huangchengsir/pipewright/internal/dbadmin"
	"github.com/huangchengsir/pipewright/internal/redisadmin"
	"github.com/huangchengsir/pipewright/internal/target"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// 支持的引擎与版本(镜像 tag 白名单;DBaaS 不透传任意 tag,防注入与漂移)。
var engineVersions = map[string][]string{
	"mysql":    {"8.0", "8.4"},
	"postgres": {"16", "17"},
	"redis":    {"7"},
}

// 引擎容器内服务端口(宿主映射目标)。
var engineInnerPort = map[string]int{
	"mysql":    3306,
	"postgres": 5432,
	"redis":    6379,
}

// 自动分配端口段(DBaaS 专属,避开 deploy 的 20000-30000 构建段)。
const (
	autoPortLo = 33000
	autoPortHi = 40000
)

var (
	// reName 实例名白名单(小写字母/数字/-,兼作 compose 项目名片段)。
	reName = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}[a-z0-9]$`)
	// rePassword 口令字符集:字母数字 + _-.~@+=:(12..64 位)。刻意排除空白/引号/
	// 反斜杠/逗号/花括号等——口令会进 compose 流序列、env 与 shell 双引号上下文,
	// 在源头关闭注入面比在各注入点逐一转义更可靠。
	rePassword = regexp.MustCompile(`^[A-Za-z0-9_\-.~@+=:]{12,64}$`)
	// reCPUs / reMemory 资源限额格式(docker compose 语义)。
	reCPUs   = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
	reMemory = regexp.MustCompile(`^[0-9]+[bkmgt]?$`)
)

// 领域错误。
var (
	ErrInvalidInput  = errors.New("dbaas: invalid input")
	ErrNotFound      = errors.New("dbaas: instance not found")
	ErrNameTaken     = errors.New("dbaas: instance name already taken")
	ErrPortAlloc     = errors.New("dbaas: no free port in range")
	ErrBusy          = errors.New("dbaas: operation already in progress")
	ErrUnsupported   = errors.New("dbaas: unsupported engine or version")
)

// Instance 是实例视图(绝无明文口令)。
type Instance struct {
	ID                 string
	Name               string
	Engine             string
	Version            string
	ServerID           string
	ServerName         string
	Host               string
	HostPort           int
	StackName          string
	CPUs               string
	Memory             string
	Status             string // creating|running|error
	StatusDetail       string
	BackupIntervalHours int
	BackupRetention    int
	LastBackupAt       string
	PasswordConfigured bool
	CreatedAt          string
	UpdatedAt          string
	// LiveStatus 是 docker ps 探测的容器实时状态(best-effort;空 = 未探测/不可达)。
	LiveStatus string
	// GeneratedPassword 仅在 Create 响应中一次性携带(HTTP 层负责不落审计/日志;
	// 其它路径恒为空)。
	GeneratedPassword string `json:"-"`
}

// CreateInput 是创建实例的入参。
type CreateInput struct {
	Name                string
	Engine              string
	Version             string
	ServerID            string
	Port                int    // 0 = 自动分配
	AdminPassword       string // 空 = 自动生成
	CPUs                string // 如 "2";空 = 不限
	Memory              string // 如 "1g";空 = 不限
	BackupIntervalHours int
	BackupRetention     int
}

// Service 是 DBaaS 领域服务。
type Service struct {
	db        *sql.DB
	targets   target.Service
	dba       dbadmin.Service
	redisa    redisadmin.Service
	vault     vault.Vault
	opt       Options

	mu       sync.Mutex
	inflight map[string]bool // 实例 id → 有备份/恢复进行中
}

// Options 装配参数。
type Options struct {
	// DataDir 控制机数据目录(备份落 <DataDir>/dbaas-backups/)。
	DataDir string
	// Uploader 可选对象存储上传(objectstore.Service 满足;nil = 禁用自动上云)。
	Uploader Uploader
	Logf     func(format string, args ...any)
}

// Uploader 把备份文件上传到对象存储(与 backup.Uploader 同形)。
type Uploader interface {
	Upload(ctx context.Context, providerID, objectKey string, body io.Reader) error
}

// New 构造服务。
func New(db *sql.DB, targets target.Service, dba dbadmin.Service, redisa redisadmin.Service, v vault.Vault, opt Options) *Service {
	if opt.Logf == nil {
		opt.Logf = log.Printf
	}
	return &Service{db: db, targets: targets, dba: dba, redisa: redisa, vault: v, opt: opt, inflight: map[string]bool{}}
}

func (s *Service) logf(format string, args ...any) { s.opt.Logf("[dbaas] "+format, args...) }

// BackupsRoot 返回备份落盘根目录。
func (s *Service) BackupsRoot() string { return filepath.Join(s.opt.DataDir, "dbaas-backups") }

// generatePassword 生成 24 位字母数字口令(与 appstore.generateSecret 同策略)。
func generatePassword() (string, error) {
	const chars = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, len(buf))
	for i, b := range buf {
		out[i] = chars[int(b)%len(chars)]
	}
	return string(out), nil
}

// validateCreate 校验创建入参,归一默认值。
func validateCreate(in *CreateInput) error {
	in.Name = strings.TrimSpace(in.Name)
	if !reName.MatchString(in.Name) {
		return fmt.Errorf("%w: 实例名须为 3..32 位小写字母/数字/-(字母开头)", ErrInvalidInput)
	}
	in.Engine = strings.TrimSpace(in.Engine)
	versions, ok := engineVersions[in.Engine]
	if !ok {
		return fmt.Errorf("%w: 引擎须为 mysql/postgres/redis", ErrInvalidInput)
	}
	in.Version = strings.TrimSpace(in.Version)
	if !contains(versions, in.Version) {
		return fmt.Errorf("%w: %s 支持版本:%s", ErrUnsupported, in.Engine, strings.Join(versions, "/"))
	}
	if in.Port != 0 && (in.Port < 1024 || in.Port > 65535) {
		return fmt.Errorf("%w: 端口须在 1024..65535", ErrInvalidInput)
	}
	if in.AdminPassword != "" && !rePassword.MatchString(in.AdminPassword) {
		return fmt.Errorf("%w: 口令须为 12..64 位可打印 ASCII(不含空白/引号/反斜杠)", ErrInvalidInput)
	}
	if in.CPUs != "" && !reCPUs.MatchString(in.CPUs) {
		return fmt.Errorf("%w: CPU 限额格式非法(如 2 / 0.5)", ErrInvalidInput)
	}
	if in.Memory != "" && !reMemory.MatchString(in.Memory) {
		return fmt.Errorf("%w: 内存限额格式非法(如 512m / 2g)", ErrInvalidInput)
	}
	if in.BackupIntervalHours < 0 || in.BackupIntervalHours > 24*30 {
		return fmt.Errorf("%w: 备份间隔须在 0..720 小时", ErrInvalidInput)
	}
	if in.BackupRetention < 0 {
		in.BackupRetention = 0
	}
	if in.BackupRetention == 0 {
		in.BackupRetention = 7
	}
	return nil
}

func contains(list []string, v string) bool {
	for _, e := range list {
		if e == v {
			return true
		}
	}
	return false
}

// Create 创建并部署一个实例(同步执行:与应用商店部署同语义,compose up -d 不设
// 主动超时——镜像拉取时长取决于带宽)。部署失败不落库(用户可直接重试)。
func (s *Service) Create(ctx context.Context, in CreateInput) (*Instance, error) {
	if err := validateCreate(&in); err != nil {
		return nil, err
	}
	if s.vault == nil {
		return nil, fmt.Errorf("dbaas: vault 未装配")
	}
	var name string
	row := s.db.QueryRowContext(ctx, `SELECT name FROM dbaas_instances WHERE name = ?`, in.Name)
	if err := row.Scan(&name); err == nil {
		return nil, ErrNameTaken
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("dbaas: check name: %w", err)
	}

	srv, err := s.targets.Get(ctx, in.ServerID)
	if err != nil {
		return nil, fmt.Errorf("dbaas: 目标服务器不存在: %w", err)
	}
	if srv.IsPVENode() {
		return nil, fmt.Errorf("%w: PVE 节点不能作为 DBaaS 部署目标", ErrInvalidInput)
	}

	password := in.AdminPassword
	if password == "" {
		if password, err = generatePassword(); err != nil {
			return nil, fmt.Errorf("dbaas: 生成口令: %w", err)
		}
	}

	port := in.Port
	if port == 0 {
		if port, err = s.allocPort(ctx, in.ServerID); err != nil {
			return nil, err
		}
	}

	stack := "db-" + in.Name
	compose, err := renderCompose(in.Engine, in.Version, in.Name, port, password)
	if err != nil {
		return nil, err
	}
	if compose, err = composeops.InjectLimits(compose, in.CPUs, in.Memory); err != nil {
		return nil, err
	}
	res := composeops.Deploy(ctx, s.targets, in.ServerID, stack, compose)
	if !res.OK {
		return nil, fmt.Errorf("dbaas: 部署失败: %s", res.Err)
	}

	// 联动登记进在线管理(best-effort:失败不影响实例本身,备注进状态详情)。
	regNote := s.autoRegister(in.Engine, in.Name, srv.Host, port, in.ServerID, password, stack)

	sealed, err := s.vault.SealSecret([]byte(password))
	if err != nil {
		return nil, fmt.Errorf("dbaas: 密封口令失败(实例已部署,vault 未配置?): %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	inst := &Instance{
		ID: "dbaas-" + uuid.NewString(), Name: in.Name, Engine: in.Engine, Version: in.Version,
		ServerID: in.ServerID, ServerName: srv.Name, Host: srv.Host, HostPort: port,
		StackName: stack, CPUs: in.CPUs, Memory: in.Memory, Status: "running",
		StatusDetail: regNote, BackupIntervalHours: in.BackupIntervalHours,
		BackupRetention: in.BackupRetention, PasswordConfigured: true,
		CreatedAt: now, UpdatedAt: now,
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO dbaas_instances (id, name, engine, version, server_id, host_port, admin_password_sealed, stack_name, cpus, memory, status, status_detail, backup_interval_hours, backup_retention, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		inst.ID, inst.Name, inst.Engine, inst.Version, inst.ServerID, inst.HostPort, sealed,
		inst.StackName, inst.CPUs, inst.Memory, inst.Status, inst.StatusDetail,
		inst.BackupIntervalHours, inst.BackupRetention, inst.CreatedAt, inst.UpdatedAt); err != nil {
		return nil, fmt.Errorf("dbaas: 落库失败(实例已部署但未登记,请手动清理栈 %s): %w", stack, err)
	}
	inst.GeneratedPassword = password // 仅创建响应一次性返回
	s.logf("实例已创建:%s(%s %s)→ %s:%d", inst.Name, inst.Engine, inst.Version, srv.Host, port)
	return inst, nil
}

// allocPort 从 33000-40000 找一个未占用端口:先查库内已用,再经 SSH /dev/tcp 探测
//(与 deploy/ports.go 同判据:退出 0 = 占用);docker 端口仲裁兜底在 compose up。
func (s *Service) allocPort(ctx context.Context, serverID string) (int, error) {
	used := map[int]bool{}
	rows, err := s.db.QueryContext(ctx, `SELECT host_port FROM dbaas_instances`)
	if err == nil {
		for rows.Next() {
			var p int
			if rows.Scan(&p) == nil {
				used[p] = true
			}
		}
		_ = rows.Close()
	}
	for p := autoPortLo; p <= autoPortHi; p++ {
		if used[p] {
			continue
		}
		probe := fmt.Sprintf("exec 3<>/dev/tcp/127.0.0.1/%d", p)
		res, perr := s.targets.Exec(ctx, serverID, []string{"bash", "-c", probe})
		if perr != nil {
			return 0, fmt.Errorf("dbaas: 端口探测失败: %w", perr)
		}
		if res.ExitCode != 0 {
			return p, nil // 连不上 = 空闲
		}
	}
	return 0, ErrPortAlloc
}

// autoRegister 联动登记在线管理(mysql/postgres → dbadmin;redis → redisadmin)。
func (s *Service) autoRegister(engine, name, host string, port int, serverID, password, stack string) string {
	timeout, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	switch engine {
	case "mysql", "postgres":
		if s.dba != nil {
			if _, err := s.dba.Register(timeout, dbadmin.RegisterInput{
				Engine: engine, Name: name, Host: host, Port: port,
				Username: engineUser(engine), Password: password,
				ServerID: serverID, StackName: stack,
			}); err != nil {
				return "在线管理登记失败(可在 设置→数据库 手动登记):" + trimErr(err)
			}
		}
	case "redis":
		if s.redisa != nil {
			if _, err := s.redisa.Register(timeout, redisadmin.RegisterInput{
				Name: name, Host: host, Port: port, Password: password,
				ServerID: serverID, StackName: stack,
			}); err != nil {
				return "在线管理登记失败(可在 设置→Redis 手动登记):" + trimErr(err)
			}
		}
	}
	return ""
}

// engineUser 是引擎的管理用户名。
func engineUser(engine string) string {
	if engine == "postgres" {
		return "postgres"
	}
	return "root"
}

func trimErr(err error) string {
	msg := err.Error()
	if len(msg) > 180 {
		msg = msg[:180] + "…"
	}
	return msg
}

// Get 按 id 取实例(行内状态;不发 SSH)。
func (s *Service) Get(ctx context.Context, id string) (*Instance, error) {
	row := s.db.QueryRowContext(ctx, instanceSelect+` WHERE i.id = ?`, id)
	return scanInstance(row)
}

// List 列出全部实例(新→老)。refresh=true 时附带 docker ps 实时状态(best-effort,
// 逐台并行探测,失败置空不报错)。
func (s *Service) List(ctx context.Context, refresh bool) ([]Instance, error) {
	rows, err := s.db.QueryContext(ctx, instanceSelect + ` ORDER BY i.created_at DESC, i.id DESC`)
	if err != nil {
		return nil, fmt.Errorf("dbaas: list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Instance
	for rows.Next() {
		var inst Instance
		if err := scanInstanceInto(rows, &inst); err != nil {
			return nil, err
		}
		out = append(out, inst)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if refresh && len(out) > 0 {
		s.refreshLive(ctx, out)
	}
	return out, nil
}

// refreshLive 并行探测各实例的容器实时状态(docker ps 按项目标签)。
func (s *Service) refreshLive(ctx context.Context, list []Instance) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	for i := range list {
		wg.Add(1)
		go func(inst *Instance) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			st, err := s.ContainerStatus(ctx, inst.ServerID, inst.StackName)
			if err == nil {
				inst.LiveStatus = st
			}
		}(&list[i])
	}
	wg.Wait()
}

// ContainerStatus 返回某栈的容器状态摘要(Up xx → running;Exited → stopped;
// 空 = 无容器)。
func (s *Service) ContainerStatus(ctx context.Context, serverID, stack string) (string, error) {
	res, err := s.targets.Exec(ctx, serverID,
		[]string{"docker", "ps", "-a", "--filter", "label=com.docker.compose.project=" + stack, "--format", "{{.Status}}"})
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(res.Stdout)
	if idx := strings.IndexAny(line, "\n"); idx >= 0 {
		line = strings.TrimSpace(line[:idx])
	}
	return line, nil
}

// Start/Stop/Restart 按 compose 项目标签取容器 ID 后纯 docker 操作(不依赖 compose CLI)。
func (s *Service) containerAction(ctx context.Context, id, action string) (*Instance, error) {
	inst, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	ids, err := composeops.ProjectContainerIDs(ctx, s.targets, inst.ServerID, inst.StackName)
	if err != nil {
		return nil, fmt.Errorf("dbaas: 取容器失败: %w", err)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("%w: 栈 %s 无容器", ErrNotFound, inst.StackName)
	}
	cmd := append([]string{"docker", action}, ids...)
	res, err := s.targets.Exec(ctx, inst.ServerID, cmd)
	if err != nil {
		return nil, fmt.Errorf("dbaas: %s 失败: %w", action, err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("dbaas: %s 失败: %s", action, strings.TrimSpace(res.Stderr))
	}
	return inst, nil
}

func (s *Service) Start(ctx context.Context, id string) (*Instance, error)   { return s.containerAction(ctx, id, "start") }
func (s *Service) Stop(ctx context.Context, id string) (*Instance, error)   { return s.containerAction(ctx, id, "stop") }
func (s *Service) Restart(ctx context.Context, id string) (*Instance, error) { return s.containerAction(ctx, id, "restart") }

// RevealPassword 解密封返回管理员口令(明文仅进程内;HTTP 层负责审计)。
func (s *Service) RevealPassword(ctx context.Context, id string) (string, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return "", err
	}
	var sealed []byte
	if err := s.db.QueryRowContext(ctx, `SELECT admin_password_sealed FROM dbaas_instances WHERE id = ?`, id).Scan(&sealed); err != nil {
		return "", err
	}
	if len(sealed) == 0 {
		return "", fmt.Errorf("dbaas: 该实例未密封口令")
	}
	plain, err := s.vault.OpenSecret(sealed)
	if err != nil {
		return "", fmt.Errorf("dbaas: 解密失败(master key 变更?): %w", err)
	}
	return string(plain), nil
}

// SetSchedule 更新备份计划(间隔/保留)。
func (s *Service) SetSchedule(ctx context.Context, id string, intervalHours, retention int) error {
	if intervalHours < 0 || intervalHours > 24*30 {
		return fmt.Errorf("%w: 备份间隔须在 0..720 小时", ErrInvalidInput)
	}
	if retention < 0 {
		retention = 0
	}
	if retention == 0 {
		retention = 7
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE dbaas_instances SET backup_interval_hours = ?, backup_retention = ?, updated_at = ? WHERE id = ?`,
		intervalHours, retention, time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete 拆除实例:容器/卷(可选保留)/受管目录 + 级联注销在线管理登记 + 删行(备份
// 记录随外键级联删;磁盘备份文件保留,由调用方决定是否另行清理)。
func (s *Service) Delete(ctx context.Context, id string, keepVolumes bool) error {
	inst, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	res := composeops.Teardown(ctx, s.targets, inst.ServerID, inst.StackName, !keepVolumes)
	if !res.OK {
		return fmt.Errorf("dbaas: 拆除失败: %s", res.Err)
	}
	s.deregister(ctx, inst)
	if _, derr := s.db.ExecContext(ctx, `DELETE FROM dbaas_instances WHERE id = ?`, id); derr != nil {
		return derr
	}
	s.logf("实例已删除:%s(卷 %s)", inst.Name, map[bool]string{true: "保留", false: "已删除"}[keepVolumes])
	return nil
}

// deregister 级联注销在线管理登记(按 engine+host+port 匹配;best-effort)。
func (s *Service) deregister(ctx context.Context, inst *Instance) {
	switch inst.Engine {
	case "mysql", "postgres":
		if s.dba != nil {
			if list, err := s.dba.List(ctx); err == nil {
				for _, e := range list {
					if e.Engine == inst.Engine && e.Host == inst.Host && e.Port == inst.HostPort {
						_ = s.dba.Delete(ctx, e.ID)
					}
				}
			}
		}
	case "redis":
		if s.redisa != nil {
			if list, err := s.redisa.List(ctx); err == nil {
				for _, e := range list {
					if e.Host == inst.Host && e.Port == inst.HostPort {
						_ = s.redisa.Delete(ctx, e.ID)
					}
				}
			}
		}
	}
}

// ── SQL 辅助 ─────────────────────────────────────────────────────────────────

const instanceSelect = `SELECT i.id, i.name, i.engine, i.version, i.server_id, COALESCE(s.name, ''), COALESCE(s.host, ''),
	i.host_port, i.stack_name, i.cpus, i.memory, i.status, i.status_detail,
	i.backup_interval_hours, i.backup_retention, i.last_backup_at, LENGTH(i.admin_password_sealed), i.created_at, i.updated_at
	FROM dbaas_instances i LEFT JOIN servers s ON s.id = i.server_id`

type rowScanner interface{ Scan(dest ...any) error }

func scanInstance(row rowScanner) (*Instance, error) {
	var inst Instance
	if err := scanInstanceInto(row, &inst); err != nil {
		return nil, err
	}
	return &inst, nil
}

func scanInstanceInto(row rowScanner, inst *Instance) error {
	var pwLen int
	if err := row.Scan(&inst.ID, &inst.Name, &inst.Engine, &inst.Version, &inst.ServerID,
		&inst.ServerName, &inst.Host, &inst.HostPort, &inst.StackName, &inst.CPUs, &inst.Memory,
		&inst.Status, &inst.StatusDetail, &inst.BackupIntervalHours, &inst.BackupRetention,
		&inst.LastBackupAt, &pwLen, &inst.CreatedAt, &inst.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	inst.PasswordConfigured = pwLen > 0
	return nil
}
