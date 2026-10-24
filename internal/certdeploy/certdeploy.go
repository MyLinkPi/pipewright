// Package certdeploy 证书部署目标管理:把证书(fullchain)/私钥下发到远程机器指定
// 路径,部署成功后可执行命令(如 `nginx -s reload`);证书签发/导入/续期后经 certmgmt
// 联动自动重新部署(实现 certmgmt.PlatformHTTPS 接口:UsesCert 占用拦截 / RedeployCert
// 联动重部署)。证书 PEM 经 CertSource 进程内解密,绝不过 HTTP、绝不落日志;远端命令
// 一律 array 化(AC-SEC-02),用户配置的 post_command 与 servercmd 批量命令同级信任,
// 经 `sh -c` 执行。
package certdeploy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/huangchengsir/pipewright/internal/target"
)

// 部署目标状态(” = 从未部署)。
const (
	StatusRunning = "running"
	StatusSuccess = "success"
	StatusFailed  = "failed"
)

// 部署触发来源(历史记录 source 列)。
const (
	SourceManual = "manual" // 页面手动触发
	SourceRenew  = "renew"  // 证书签发/导入/续期联动
)

// HistoryKeep 是每个部署目标保留的历史条数;插入后裁掉更早的(防 detail 无限膨胀)。
const HistoryKeep = 50

const (
	// allDeployTimeout 是联动重部署(一证书多目标扇出)的总预算;certmgmt 联动在签发
	// goroutine 的 10 分钟窗口内执行,这里再钳一层避免挤占后续联动。
	allDeployTimeout = 5 * time.Minute
	// fanoutCap 是多目标并发部署的信号量上限(对齐 deploy 包扇出)。
	fanoutCap = 4
	// privTTL 是免密 sudo 探测结果的缓存时长(对齐 diskadmin)。
	privTTL = 10 * time.Minute
	// maxCommandLen 是 post_command 的长度上限。
	maxCommandLen = 2000
)

// 哨兵错误(HTTP 层据此映射 404/422/500)。
var (
	ErrNotFound     = errors.New("certdeploy: 部署目标不存在")
	ErrCertNotFound = errors.New("certdeploy: 证书不存在")
	ErrInvalidInput = errors.New("certdeploy: 入参无效")
	ErrDeployFailed = errors.New("certdeploy: 部署失败")
)

// Target 是一个证书部署目标:证书 → 指定服务器上的指定路径 + 部署后命令。
type Target struct {
	ID             string
	CertID         string
	ServerID       string
	Name           string // 备注名(展示用;空则回退「服务器名」)
	CertPath       string // 远端证书(fullchain)绝对路径,落位后 chmod 644
	KeyPath        string // 远端私钥绝对路径,落位后 chmod 600
	PostCommand    string
	Enabled        bool
	Status         string // '' | running | success | failed(最近一次)
	StatusDetail   string
	LastDeployedAt time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time

	// 冗余展示(list 查询时 join 填充;非权威)。
	ServerName        string
	CertPrimaryDomain string
}

// Run 是一次部署的历史记录。
type Run struct {
	ID         string
	DeployID   string
	ServerID   string
	ServerName string
	Source     string // manual | renew
	Status     string // success | failed
	Detail     string
	StartedAt  time.Time
	FinishedAt time.Time
}

// CertSource 进程内解密证书 PEM(main.go 适配 certmgmt 注入,避免包环)。
type CertSource interface {
	OpenCertPEM(ctx context.Context, certID string) (certPEM, keyPEM string, err error)
}

// Executor 是部署所需的远端执行面(收窄 target.Service 便于单测 stub)。
type Executor interface {
	Exec(ctx context.Context, serverID string, cmd []string) (*target.ExecResult, error)
	Upload(ctx context.Context, serverID string, content io.Reader, remotePath string) error
	Get(ctx context.Context, id string) (*target.Server, error)
}

// CreateInput 创建入参。
type CreateInput struct {
	CertID      string
	ServerID    string
	Name        string
	CertPath    string
	KeyPath     string
	PostCommand string
	Enabled     *bool
}

// UpdateInput 更新入参(指针字段 nil = 不改)。
type UpdateInput struct {
	Name        *string
	CertPath    *string
	KeyPath     *string
	PostCommand *string
	Enabled     *bool
}

// Service 管理证书部署目标并执行部署。
type Service struct {
	db    *sql.DB
	exec  Executor
	certs CertSource
	logf  func(format string, args ...any)

	mu   sync.Mutex
	priv map[string]privEntry // 免密 sudo 探测缓存(serverID → 前缀)
}

type privEntry struct {
	prefix []string
	at     time.Time
}

// New 构造服务。exec 传 target.Service,certs 传 certmgmt 适配器。
func New(db *sql.DB, exec Executor, certs CertSource) *Service {
	return &Service{
		db:    db,
		exec:  exec,
		certs: certs,
		priv:  map[string]privEntry{},
		logf:  func(format string, args ...any) {}, // main.go 用 WithLogger 覆盖
	}
}

// WithLogger 注入日志函数。
func (s *Service) WithLogger(fn func(format string, args ...any)) *Service {
	if fn != nil {
		s.logf = fn
	}
	return s
}

func newID() string { return "certdep-" + uuid.NewString() }

// ---------- 校验 ----------

// validatePath 校验远端绝对路径:非空、以 / 开头、不是根目录本身。
func validatePath(p string) error {
	if p == "" || !strings.HasPrefix(p, "/") {
		return fmt.Errorf("%w:路径必须是以 / 开头的绝对路径", ErrInvalidInput)
	}
	if p == "/" {
		return fmt.Errorf("%w:路径不能是根目录", ErrInvalidInput)
	}
	return nil
}

// validatePaths 校验证书/私钥路径且二者不同(同路径会互相覆盖)。
func validatePaths(certPath, keyPath string) error {
	if err := validatePath(certPath); err != nil {
		return fmt.Errorf("证书路径无效:%w", err)
	}
	if err := validatePath(keyPath); err != nil {
		return fmt.Errorf("私钥路径无效:%w", err)
	}
	if certPath == keyPath {
		return fmt.Errorf("%w:证书与私钥路径不能相同", ErrInvalidInput)
	}
	return nil
}

// ---------- CRUD ----------

// List 返回部署目标(可按 certID 筛选;join servers/certificates 填冗余名)。
func (s *Service) List(ctx context.Context, certID string) ([]*Target, error) {
	q := `SELECT d.id, d.cert_id, d.server_id, d.name, d.cert_path, d.key_path,
	             d.post_command, d.enabled, d.status, d.status_detail, d.last_deployed_at,
	             d.created_at, d.updated_at,
	             COALESCE(sv.name, ''), COALESCE(c.primary_domain, '')
	      FROM cert_deploys d
	      LEFT JOIN servers sv ON sv.id = d.server_id
	      LEFT JOIN certificates c ON c.id = d.cert_id`
	var args []any
	if certID != "" {
		q += " WHERE d.cert_id = ?"
		args = append(args, certID)
	}
	q += " ORDER BY d.created_at ASC, d.id ASC"

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("certdeploy: list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*Target
	for rows.Next() {
		d, err := scanTarget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func scanTarget(row interface{ Scan(dest ...any) error }) (*Target, error) {
	var d Target
	var enabled int
	var lastDeployed, created, updated string
	if err := row.Scan(&d.ID, &d.CertID, &d.ServerID, &d.Name, &d.CertPath, &d.KeyPath,
		&d.PostCommand, &enabled, &d.Status, &d.StatusDetail, &lastDeployed,
		&created, &updated, &d.ServerName, &d.CertPrimaryDomain); err != nil {
		return nil, fmt.Errorf("certdeploy: scan: %w", err)
	}
	d.Enabled = enabled != 0
	d.LastDeployedAt = parseTime(lastDeployed)
	d.CreatedAt = parseTime(created)
	d.UpdatedAt = parseTime(updated)
	return &d, nil
}

// Get 返回单个部署目标。
func (s *Service) Get(ctx context.Context, id string) (*Target, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT d.id, d.cert_id, d.server_id, d.name, d.cert_path, d.key_path,
		        d.post_command, d.enabled, d.status, d.status_detail, d.last_deployed_at,
		        d.created_at, d.updated_at,
		        COALESCE(sv.name, ''), COALESCE(c.primary_domain, '')
		 FROM cert_deploys d
		 LEFT JOIN servers sv ON sv.id = d.server_id
		 LEFT JOIN certificates c ON c.id = d.cert_id
		 WHERE d.id = ?`, id)
	d, err := scanTarget(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return d, err
}

// Create 登记部署目标(校验证书/服务器存在、路径合法)。
func (s *Service) Create(ctx context.Context, in CreateInput) (*Target, error) {
	if in.CertID == "" || in.ServerID == "" {
		return nil, fmt.Errorf("%w:certId/serverId 不能为空", ErrInvalidInput)
	}
	if err := validatePaths(in.CertPath, in.KeyPath); err != nil {
		return nil, err
	}
	if len(in.PostCommand) > maxCommandLen {
		return nil, fmt.Errorf("%w:部署后命令过长(上限 %d 字符)", ErrInvalidInput, maxCommandLen)
	}
	// 证书/服务器存在性(FK 只护 server_id;cert_id 逻辑引用须显式校验)。
	var one string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM certificates WHERE id = ?`, in.CertID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrCertNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("certdeploy: 查证书: %w", err)
	}
	if _, err := s.exec.Get(ctx, in.ServerID); err != nil {
		return nil, fmt.Errorf("%w:服务器不可用: %v", ErrInvalidInput, err)
	}

	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	now := time.Now().UTC()
	d := &Target{
		ID:          newID(),
		CertID:      in.CertID,
		ServerID:    in.ServerID,
		Name:        strings.TrimSpace(in.Name),
		CertPath:    in.CertPath,
		KeyPath:     in.KeyPath,
		PostCommand: in.PostCommand,
		Enabled:     enabled,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO cert_deploys
		 (id, cert_id, server_id, name, cert_path, key_path, post_command, enabled,
		  status, status_detail, last_deployed_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', '', '', ?, ?)`,
		d.ID, d.CertID, d.ServerID, d.Name, d.CertPath, d.KeyPath, d.PostCommand,
		boolToInt(d.Enabled), formatTime(d.CreatedAt), formatTime(d.UpdatedAt)); err != nil {
		return nil, fmt.Errorf("certdeploy: insert: %w", err)
	}
	return s.Get(ctx, d.ID)
}

// Update 改备注/路径/部署后命令/启用开关(cert_id、server_id 不可改:换目标 = 新建)。
func (s *Service) Update(ctx context.Context, id string, in UpdateInput) (*Target, error) {
	d, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	certPath, keyPath := d.CertPath, d.KeyPath
	if in.CertPath != nil {
		certPath = *in.CertPath
	}
	if in.KeyPath != nil {
		keyPath = *in.KeyPath
	}
	if err := validatePaths(certPath, keyPath); err != nil {
		return nil, err
	}
	postCommand := d.PostCommand
	if in.PostCommand != nil {
		postCommand = *in.PostCommand
		if len(postCommand) > maxCommandLen {
			return nil, fmt.Errorf("%w:部署后命令过长(上限 %d 字符)", ErrInvalidInput, maxCommandLen)
		}
	}
	sets := []string{"cert_path = ?", "key_path = ?", "post_command = ?", "updated_at = ?"}
	args := []any{certPath, keyPath, postCommand, formatTime(time.Now().UTC())}
	if in.Name != nil {
		sets = append(sets, "name = ?")
		args = append(args, strings.TrimSpace(*in.Name))
	}
	if in.Enabled != nil {
		sets = append(sets, "enabled = ?")
		args = append(args, boolToInt(*in.Enabled))
	}
	args = append(args, id)
	res, err := s.db.ExecContext(ctx,
		"UPDATE cert_deploys SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	if err != nil {
		return nil, fmt.Errorf("certdeploy: update: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return s.Get(ctx, id)
}

// Delete 删除部署目标(先删历史再删本体,不依赖 FK 级联)。
func (s *Service) Delete(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("certdeploy: delete begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM cert_deploy_runs WHERE deploy_id = ?`, id); err != nil {
		return fmt.Errorf("certdeploy: delete runs: %w", err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM cert_deploys WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("certdeploy: delete: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// UsesCert 报告证书是否被任一部署目标引用(含停用目标:存在即占用,防删证书悬挂)。
func (s *Service) UsesCert(ctx context.Context, certID string) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM cert_deploys WHERE cert_id = ?`, certID).Scan(&n); err != nil {
		return false, fmt.Errorf("certdeploy: uses cert: %w", err)
	}
	return n > 0, nil
}

// RedeployCert 联动重部署该证书的全部启用目标(certmgmt.PlatformHTTPS 接口)。
func (s *Service) RedeployCert(ctx context.Context, certID string) error {
	return s.DeployAllForCert(ctx, certID, SourceRenew)
}

// ListRuns 返回部署目标的执行历史(新→旧;limit 0 = 默认 50)。
func (s *Service) ListRuns(ctx context.Context, deployID string, limit int) ([]*Run, error) {
	if limit <= 0 || limit > HistoryKeep {
		limit = HistoryKeep
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, deploy_id, server_id, server_name, source, status, detail, started_at, finished_at
		 FROM cert_deploy_runs WHERE deploy_id = ?
		 ORDER BY started_at DESC, id DESC LIMIT ?`, deployID, limit)
	if err != nil {
		return nil, fmt.Errorf("certdeploy: list runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*Run
	for rows.Next() {
		var r Run
		var started, finished string
		if err := rows.Scan(&r.ID, &r.DeployID, &r.ServerID, &r.ServerName, &r.Source,
			&r.Status, &r.Detail, &started, &finished); err != nil {
			return nil, fmt.Errorf("certdeploy: scan run: %w", err)
		}
		r.StartedAt = parseTime(started)
		r.FinishedAt = parseTime(finished)
		out = append(out, &r)
	}
	return out, rows.Err()
}

// ---------- 部署编排 ----------

// DeployOne 手动部署单个目标(同步执行,超时经 ctx 由 HTTP 层控制)。部署执行失败时
// 错误上抛,同时返回落库后的最新行(前端可展示 failed + statusDetail)。
func (s *Service) DeployOne(ctx context.Context, id string) (*Target, error) {
	d, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if derr := s.deployOne(ctx, d, SourceManual); derr != nil {
		nd, gerr := s.Get(ctx, id)
		if gerr != nil {
			return nil, derr
		}
		return nd, derr
	}
	return s.Get(ctx, id)
}

// DeployAllForCert 并发部署该证书的全部启用目标(签发/导入/续期联动与批量手动入口;
// 单目标失败不连累其它,聚合错误人话返回;无可部署目标 = no-op)。
func (s *Service) DeployAllForCert(ctx context.Context, certID, source string) error {
	targets, err := s.List(ctx, certID)
	if err != nil {
		return err
	}
	var active []*Target
	for _, d := range targets {
		if d.Enabled {
			active = append(active, d)
		}
	}
	if len(active) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, allDeployTimeout)
	defer cancel()

	sem := make(chan struct{}, fanoutCap)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failed []string
	for i := range active {
		wg.Add(1)
		go func(d *Target) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					mu.Lock()
					failed = append(failed, displayName(d)+"(panic:"+fmt.Sprint(r)+")")
					mu.Unlock()
				}
			}()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := s.deployOne(ctx, d, source); err != nil {
				mu.Lock()
				failed = append(failed, displayName(d)+":"+truncate(err.Error(), 200))
				mu.Unlock()
			}
		}(active[i])
	}
	wg.Wait()
	if len(failed) > 0 {
		return fmt.Errorf("%w:%d/%d 个目标部署失败 → %s",
			ErrDeployFailed, len(failed), len(active), strings.Join(failed, "、"))
	}
	return nil
}

// deployOne 部署单个目标并落状态 + 历史;错误已含人话 detail。
func (s *Service) deployOne(ctx context.Context, d *Target, source string) error {
	started := time.Now().UTC()
	_ = s.setStatus(ctx, d.ID, StatusRunning, "")

	err := s.doDeploy(ctx, d)
	status, detail := StatusSuccess, ""
	if err != nil {
		status = StatusFailed
		detail = truncate(err.Error(), 2000)
		s.logf("[certdeploy] 目标 %s(%s)部署失败:%s", d.ID, displayName(d), detail)
	}
	finished := time.Now().UTC()
	_ = s.finishDeploy(ctx, d.ID, status, detail, finished)
	s.recordRun(ctx, &Run{
		ID:         "certdeprun-" + uuid.NewString(),
		DeployID:   d.ID,
		ServerID:   d.ServerID,
		ServerName: d.ServerName,
		Source:     source,
		Status:     status,
		Detail:     detail,
		StartedAt:  started,
		FinishedAt: finished,
	})
	if err != nil {
		return fmt.Errorf("%w:%s", ErrDeployFailed, detail)
	}
	return nil
}

// doDeploy 是单目标部署的执行序列:解密 → 上传临时文件 → 落位 → 部署后命令。
func (s *Service) doDeploy(ctx context.Context, d *Target) error {
	sv, err := s.exec.Get(ctx, d.ServerID)
	if err != nil {
		return fmt.Errorf("服务器不可用:%v", err)
	}
	certPEM, keyPEM, err := s.certs.OpenCertPEM(ctx, d.CertID)
	if err != nil {
		return fmt.Errorf("证书未就绪(未签发/未导入或解密失败):%v", err)
	}
	prefix, err := s.privilegePrefix(ctx, d.ServerID, sv.User)
	if err != nil {
		return err
	}

	// 临时文件固定名(/tmp 全局可写);部署前清残留,落位命令成功后再删,失败可重试。
	tmpCert := "/tmp/pw-certdeploy-" + d.ID + "-cert.tmp"
	tmpKey := "/tmp/pw-certdeploy-" + d.ID + "-key.tmp"
	_, _ = s.execWithPrefix(ctx, d.ServerID, prefix, []string{"rm", "-f", tmpCert, tmpKey})
	if err := s.exec.Upload(ctx, d.ServerID, strings.NewReader(certPEM), tmpCert); err != nil {
		return fmt.Errorf("上传证书临时文件失败:%v", err)
	}
	if err := s.exec.Upload(ctx, d.ServerID, strings.NewReader(keyPEM), tmpKey); err != nil {
		return fmt.Errorf("上传私钥临时文件失败:%v", err)
	}
	if err := s.installFile(ctx, d.ServerID, prefix, tmpCert, d.CertPath, "644"); err != nil {
		return fmt.Errorf("落位证书失败:%v(若目标目录需要 root,请用 root 登录或配置免密 sudo)", err)
	}
	if err := s.installFile(ctx, d.ServerID, prefix, tmpKey, d.KeyPath, "600"); err != nil {
		return fmt.Errorf("落位私钥失败:%v(若目标目录需要 root,请用 root 登录或配置免密 sudo)", err)
	}
	if d.PostCommand != "" {
		if err := s.execWithPrefixChecked(ctx, d.ServerID, prefix, "部署后命令", []string{"sh", "-c", d.PostCommand}); err != nil {
			return err
		}
	}
	return nil
}

// installFile 把远端临时文件原子落位到目标路径:mkdir 父目录 → mv(同文件系统原子替换,
// 半写状态绝不会被服务进程读到)→ chmod → 清临时文件。命令模板为常量、路径只作位置参数
// (经 shellQuote 转义,防注入,对齐 target.Upload 先例);prefix 为提权 argv 前缀。
func (s *Service) installFile(ctx context.Context, serverID string, prefix []string, src, dst, mode string) error {
	cmd := []string{"sh", "-c",
		`mkdir -p "$(dirname "$1")" && mv "$0" "$1" && chmod "$2" "$1" && rm -f "$0"`,
		src, dst, mode}
	return s.execWithPrefixChecked(ctx, serverID, prefix, "落位文件", cmd)
}

// privilegePrefix 返回该服务器的提权 argv 前缀(结果按服务器缓存 privTTL):
//
//	登录用户为 root → 空(直跑);
//	否则探测 sudo -n true(exit 0)→ ["sudo","-n"];
//	皆不可用 → 空前缀直跑(普通用户可写路径仍可部署;系统路径失败时由落位错误带指引)。
//
// 证书部署不强制 root(diskadmin 磁盘管理才强制),但落位到 /etc 等目录需要免密 sudo。
func (s *Service) privilegePrefix(ctx context.Context, serverID, user string) ([]string, error) {
	if user == "root" {
		return nil, nil
	}
	s.mu.Lock()
	if e, ok := s.priv[serverID]; ok && time.Since(e.at) < privTTL {
		s.mu.Unlock()
		return e.prefix, nil
	}
	s.mu.Unlock()

	res, err := s.exec.Exec(ctx, serverID, []string{"sudo", "-n", "true"})
	var prefix []string
	if err == nil && res.ExitCode == 0 {
		prefix = []string{"sudo", "-n"}
	}
	s.mu.Lock()
	s.priv[serverID] = privEntry{prefix: prefix, at: time.Now()}
	s.mu.Unlock()
	return prefix, nil
}

// execWithPrefix 加提权前缀执行,忽略结果(清理类命令)。
func (s *Service) execWithPrefix(ctx context.Context, serverID string, prefix, cmd []string) (*target.ExecResult, error) {
	full := append(append([]string{}, prefix...), cmd...)
	return s.exec.Exec(ctx, serverID, full)
}

// execWithPrefixChecked 加提权前缀执行并校验退出码;失败信息含 stderr/stdout 尾部
// (远端输出不含凭据/PEM,上界 2000 字符)。
func (s *Service) execWithPrefixChecked(ctx context.Context, serverID string, prefix []string, what string, cmd []string) error {
	res, err := s.execWithPrefix(ctx, serverID, prefix, cmd)
	if err != nil {
		return fmt.Errorf("%s执行失败:%v", what, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s退出码 %d%s", what, res.ExitCode, tailOutput(res))
	}
	return nil
}

// ---------- 状态/历史落库 ----------

func (s *Service) setStatus(ctx context.Context, id, status, detail string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE cert_deploys SET status = ?, status_detail = ?, updated_at = ? WHERE id = ?`,
		status, detail, formatTime(time.Now().UTC()), id)
	return err
}

func (s *Service) finishDeploy(ctx context.Context, id, status, detail string, finished time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE cert_deploys SET status = ?, status_detail = ?, last_deployed_at = ?, updated_at = ? WHERE id = ?`,
		status, detail, formatTime(finished), formatTime(finished), id)
	return err
}

// recordRun 插入部署历史并裁剪保留窗口(失败只记日志,不影响部署结果)。
func (s *Service) recordRun(ctx context.Context, r *Run) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.logf("[certdeploy] 记录部署历史失败: %v", err)
		return
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO cert_deploy_runs
		 (id, deploy_id, server_id, server_name, source, status, detail, started_at, finished_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.DeployID, r.ServerID, r.ServerName, r.Source, r.Status, r.Detail,
		formatTime(r.StartedAt), formatTime(r.FinishedAt)); err != nil {
		s.logf("[certdeploy] 记录部署历史失败: %v", err)
		return
	}
	// 边界法裁剪(对齐 servercmd:定位第 HistoryKeep+1 新的边界行再删窗口;避开 MySQL
	// 不支持的 `LIMIT -1` 与 IN 子查询带 LIMIT 两种方言差异)。
	var bts, bid string
	err = tx.QueryRowContext(ctx,
		`SELECT started_at, id FROM cert_deploy_runs WHERE deploy_id = ?
		 ORDER BY started_at DESC, id DESC LIMIT 1 OFFSET ?`, r.DeployID, HistoryKeep).Scan(&bts, &bid)
	if err == nil {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM cert_deploy_runs WHERE deploy_id = ?
			   AND (started_at < ? OR (started_at = ? AND id <= ?))`, r.DeployID, bts, bts, bid); err != nil {
			s.logf("[certdeploy] 裁剪部署历史失败: %v", err)
			return
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		s.logf("[certdeploy] 定位部署历史边界失败: %v", err)
		return
	}
	if err := tx.Commit(); err != nil {
		s.logf("[certdeploy] 提交部署历史失败: %v", err)
	}
}

// ---------- 辅助 ----------

func displayName(d *Target) string {
	if d.Name != "" {
		return d.Name
	}
	if d.ServerName != "" {
		return d.ServerName
	}
	return d.ServerID
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// parseTime 解析 RFC3339(Nano)落库串;坏数据回退零值(与 audit/servercmd 同容错策略)。
func parseTime(s string) time.Time {
	if ts, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return ts
	}
	if ts, err := time.Parse(time.RFC3339, s); err == nil {
		return ts
	}
	return time.Time{}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// tailOutput 拼装 stderr/stdout 尾部(每段 ≤2000 字符;远端输出不含凭据/PEM)。
func tailOutput(res *target.ExecResult) string {
	tail := func(s string) string {
		s = strings.TrimRight(s, "\n")
		if len(s) > 2000 {
			s = "…" + s[len(s)-2000:]
		}
		return s
	}
	var b strings.Builder
	if t := tail(res.Stderr); t != "" {
		b.WriteString("\nstderr: " + t)
	}
	if t := tail(res.Stdout); t != "" {
		b.WriteString("\nstdout: " + t)
	}
	return b.String()
}
