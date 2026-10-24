package secscan

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// vaultService 是本包对保险库的最小依赖(SealSecret/OpenSecret;由 vault.Vault 满足)。
type vaultService interface {
	SealSecret(plaintext []byte) ([]byte, error)
	OpenSecret(sealed []byte) ([]byte, error)
}

// validKind 报告扫描器类型枚举是否受支持。
func validKind(k string) bool { return k == KindTrivyServer || k == KindNucleiRunner }

// sealToken 密封可选 token;空 token → 空 BLOB(nil)。保险库未配置且 token 非空 → 报错。
func (s *service) sealToken(token string) ([]byte, error) {
	if token == "" {
		return nil, nil
	}
	if s.vault == nil {
		return nil, ErrVaultUnconfigured
	}
	return s.vault.SealSecret([]byte(token))
}

// openToken 解封可选 token;空 BLOB → 空串。
func (s *service) openToken(sealed []byte) (string, error) {
	if len(sealed) == 0 {
		return "", nil
	}
	if s.vault == nil {
		return "", ErrVaultUnconfigured
	}
	raw, err := s.vault.OpenSecret(sealed)
	if err != nil {
		return "", ErrVaultUnconfigured
	}
	return string(raw), nil
}

// isUniqueViolation 判断是否 kind+host+port 唯一索引冲突(双方言文本匹配)。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToUpper(err.Error())
	return strings.Contains(msg, "IDX_SCANNER_INSTANCES_ENDPOINT") ||
		strings.Contains(msg, "UNIQUE CONSTRAINT FAILED: SCANNER_INSTANCES") ||
		strings.Contains(msg, "DUPLICATE ENTRY")
}

// ListScanners 返回全部扫描器(创建时间倒序)。
func (s *service) ListScanners(ctx context.Context) ([]Instance, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, kind, name, host, port, source, server_id, stack_name, created_at, updated_at, LENGTH(token_sealed)
		 FROM scanner_instances ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("secscan: list scanners: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]Instance, 0)
	for rows.Next() {
		inst, err := scanInstance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *inst)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("secscan: iterate scanners: %w", err)
	}
	return out, nil
}

// GetScanner 读取单条;不存在 → ErrNotFound。
func (s *service) GetScanner(ctx context.Context, id string) (*Instance, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, kind, name, host, port, source, server_id, stack_name, created_at, updated_at, LENGTH(token_sealed)
		 FROM scanner_instances WHERE id = ?`, id)
	inst, err := scanInstance(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return inst, err
}

// scanner 抽象 *sql.Row 与 *sql.Rows 的 Scan。
type scanner interface {
	Scan(dest ...any) error

}

// scanInstance 把一行扫描为 Instance(token 只读密文长度,绝不解封)。
func scanInstance(sc scanner) (*Instance, error) {
	var (
		inst          Instance
		tokenLen      int
		createdAtRaw  string
		updatedAtRaw  string
	)
	if err := sc.Scan(&inst.ID, &inst.Kind, &inst.Name, &inst.Host, &inst.Port, &inst.Source,
		&inst.ServerID, &inst.StackName, &createdAtRaw, &updatedAtRaw, &tokenLen); err != nil {
		return nil, err
	}
	inst.TokenConfigured = tokenLen > 0
	var err error
	if inst.CreatedAt, err = time.Parse(time.RFC3339, createdAtRaw); err != nil {
		return nil, fmt.Errorf("secscan: parse created_at: %w", err)
	}
	if inst.UpdatedAt, err = time.Parse(time.RFC3339, updatedAtRaw); err != nil {
		return nil, fmt.Errorf("secscan: parse updated_at: %w", err)
	}
	return &inst, nil
}

// validateEndpoint 校验名称/主机/端口。
func validateEndpoint(name, host string, port int) error {
	if strings.TrimSpace(name) == "" {
		return ErrEmptyName
	}
	if strings.TrimSpace(host) == "" {
		return ErrEmptyHost
	}
	if port < 1 || port > 65535 {
		return ErrInvalidPort
	}
	return nil
}

// blobOrEmpty 把 nil 密文归一为空 BLOB(双方言 NOT NULL 列不接受 NULL)。
func blobOrEmpty(sealed []byte) []byte {
	if sealed == nil {
		return []byte{}
	}
	return sealed
}

// CreateScanner 手工登记(密封可选 token)→ 落库;同类型同地址已存在 → ErrEndpointTaken。
func (s *service) CreateScanner(ctx context.Context, in CreateInput) (*Instance, error) {
	in.Name, in.Host = strings.TrimSpace(in.Name), strings.TrimSpace(in.Host)
	if err := validateEndpoint(in.Name, in.Host, in.Port); err != nil {
		return nil, err
	}
	sealed, err := s.sealToken(in.Token)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	inst := &Instance{
		ID: uuid.NewString(), Kind: KindTrivyServer, Name: in.Name, Host: in.Host, Port: in.Port,
		TokenConfigured: in.Token != "", Source: SourceManual, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO scanner_instances (id, kind, name, host, port, token_sealed, source, server_id, stack_name, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, 'manual', '', '', ?, ?)`,
		inst.ID, inst.Kind, inst.Name, inst.Host, inst.Port, blobOrEmpty(sealed),
		now.Format(time.RFC3339), now.Format(time.RFC3339),
	); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrEndpointTaken
		}
		return nil, fmt.Errorf("secscan: insert scanner: %w", err)
	}
	return inst, nil
}

// UpdateScanner 编辑扫描器;Token 非 nil 即轮换(空串 = 清除)。
func (s *service) UpdateScanner(ctx context.Context, id string, in UpdateInput) (*Instance, error) {
	cur, err := s.GetScanner(ctx, id)
	if err != nil {
		return nil, err
	}
	next := *cur
	if in.Name != nil {
		next.Name = strings.TrimSpace(*in.Name)
	}
	if in.Host != nil {
		next.Host = strings.TrimSpace(*in.Host)
	}
	if in.Port != nil {
		next.Port = *in.Port
	}
	if err := validateEndpoint(next.Name, next.Host, next.Port); err != nil {
		return nil, err
	}
	var sealed []byte
	if in.Token != nil {
		if sealed, err = s.sealToken(*in.Token); err != nil {
			return nil, err
		}
		next.TokenConfigured = *in.Token != ""
	} else {
		// 未轮换令牌 → 保持库中既有密文原样。
		if err := s.db.QueryRowContext(ctx,
			`SELECT token_sealed FROM scanner_instances WHERE id = ?`, id).Scan(&sealed); err != nil {
			return nil, fmt.Errorf("secscan: load token: %w", err)
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE scanner_instances SET name = ?, host = ?, port = ?, token_sealed = ?, updated_at = ? WHERE id = ?`,
		next.Name, next.Host, next.Port, blobOrEmpty(sealed), now, id,
	); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrEndpointTaken
		}
		return nil, fmt.Errorf("secscan: update scanner: %w", err)
	}
	next.UpdatedAt = time.Now().UTC()
	return &next, nil
}

// DeleteScanner 删除扫描器登记(不影响已部署实例本身)。
func (s *service) DeleteScanner(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM scanner_instances WHERE id = ?`, id); err != nil {
		return fmt.Errorf("secscan: delete scanner: %w", err)
	}
	return nil
}

// Register 应用市场联动登记:同 kind+host+port 已有条目 → 原地轮换 token(未提供则清除),
// 否则新建 source=appstore 条目。trivy-server 须带服务端口;nuclei-runner 无服务端口
//(Port 固定 0,按 kind+host 唯一)。
func (s *service) Register(ctx context.Context, in RegisterInput) (*RegisterResult, error) {
	in.Host = strings.TrimSpace(in.Host)
	if !validKind(in.Kind) {
		return nil, ErrInvalidKind
	}
	if in.Kind == KindNucleiRunner {
		in.Port = 0
	}
	if in.Host == "" || strings.TrimSpace(in.Name) == "" ||
		(in.Kind == KindTrivyServer && (in.Port < 1 || in.Port > 65535)) {
		return nil, fmt.Errorf("%w: register fields", ErrInvalidInput)
	}
	sealed, err := s.sealToken(in.Token)
	if err != nil {
		return nil, err
	}
	var existingID string
	err = s.db.QueryRowContext(ctx,
		`SELECT id FROM scanner_instances WHERE kind = ? AND host = ? AND port = ?`,
		in.Kind, in.Host, in.Port,
	).Scan(&existingID)
	now := time.Now().UTC().Format(time.RFC3339)
	if err == nil {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE scanner_instances SET token_sealed = ?, stack_name = ?, updated_at = ? WHERE id = ?`,
			blobOrEmpty(sealed), in.StackName, now, existingID); err != nil {
			return nil, fmt.Errorf("secscan: register rotate: %w", err)
		}
		inst, err := s.GetScanner(ctx, existingID)
		if err != nil {
			return nil, err
		}
		return &RegisterResult{Instance: inst, Updated: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("secscan: register lookup: %w", err)
	}
	id := uuid.NewString()
	nowT := time.Now().UTC()
	inst := &Instance{
		ID: id, Kind: in.Kind, Name: strings.TrimSpace(in.Name), Host: in.Host, Port: in.Port,
		TokenConfigured: in.Token != "", Source: SourceAppStore,
		ServerID: in.ServerID, StackName: in.StackName, CreatedAt: nowT, UpdatedAt: nowT,
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO scanner_instances (id, kind, name, host, port, token_sealed, source, server_id, stack_name, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, 'appstore', ?, ?, ?, ?)`,
		id, inst.Kind, inst.Name, inst.Host, inst.Port, blobOrEmpty(sealed), in.ServerID, in.StackName, now, now,
	); err != nil {
		if isUniqueViolation(err) {
			return s.Register(ctx, in) // 并发窗口:命中唯一索引则按更新语义重试一次
		}
		return nil, fmt.Errorf("secscan: register insert: %w", err)
	}
	return &RegisterResult{Instance: inst, Updated: false}, nil
}

// TestScanner 测试已存扫描器:解封 token → /healthz;失败回落 TCP 探活(区分「服务未就绪」
// 与「端口不通」)。错误体绝不含 token。
func (s *service) TestScanner(ctx context.Context, id string) (*TestResult, error) {
	inst, err := s.GetScanner(ctx, id)
	if err != nil {
		return nil, err
	}
	var sealed []byte
	if err := s.db.QueryRowContext(ctx,
		`SELECT token_sealed FROM scanner_instances WHERE id = ?`, id).Scan(&sealed); err != nil {
		return nil, fmt.Errorf("secscan: load token: %w", err)
	}
	token, err := s.openToken(sealed)
	if err != nil {
		return nil, err
	}
	// nuclei-runner 不是网络服务:探活 = 到所在主机 docker ps 找执行容器。
	if inst.Kind == KindNucleiRunner {
		return s.probeNucleiRunner(ctx, inst), nil
	}
	return probeTrivyServer(ctx, inst.Host, inst.Port, token), nil
}

// probeNucleiRunner 验证 runner 执行容器在所在主机上运行(docker ps 按栈名过滤)。
func (s *service) probeNucleiRunner(ctx context.Context, inst *Instance) *TestResult {
	if s.runner == nil {
		return &TestResult{Err: ErrRunnerUnavailable, Detail: "扫描执行器未装配,无法验证 runner"}
	}
	res, err := s.runner.Exec(ctx, inst.ServerID, nucleiRunnerPSCmd(inst.StackName))
	if err != nil {
		return &TestResult{Err: err, Detail: "无法连接 runner 所在主机:" + humanProbeError(err)}
	}
	if res.ExitCode != 0 {
		return &TestResult{Detail: "目标机 docker 不可用,无法验证 runner"}
	}
	if strings.TrimSpace(res.Stdout) == "" {
		return &TestResult{Detail: "runner 容器未在运行(可能未部署或已停止)"}
	}
	first := res.Stdout
	if i := strings.IndexByte(res.Stdout, '\n'); i >= 0 {
		first = res.Stdout[:i]
	}
	return &TestResult{OK: true, LatencyMs: 0, Detail: "Nuclei Runner 容器运行中:" + strings.TrimSpace(first)}
}

// TestScannerDirect 用未保存接入点直连测试(token 用完即弃)。
func (s *service) TestScannerDirect(ctx context.Context, in ConnectInput) (*TestResult, error) {
	in.Host = strings.TrimSpace(in.Host)
	if in.Host == "" || in.Port < 1 || in.Port > 65535 {
		return nil, fmt.Errorf("%w: endpoint", ErrInvalidInput)
	}
	return probeTrivyServer(ctx, in.Host, in.Port, in.Token), nil
}

// probeTrivyServer 对 trivy-server 做 /healthz 探活(带可选 token),失败回落 TCP 拨号。
func probeTrivyServer(ctx context.Context, host string, port int, token string) *TestResult {
	start := time.Now()
	cctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	url := fmt.Sprintf("http://%s/healthz", net.JoinHostPort(host, fmt.Sprint(port)))
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return &TestResult{Err: err, Detail: "构造健康检查请求失败"}
	}
	if token != "" {
		req.Header.Set("Trivy-Token", token)
	}
	// 明文 HTTP:trivy-server 默认无 TLS;内网接入场景与 Stacks 明文 host:port 同姿态。
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}, Timeout: 6 * time.Second}
	resp, err := client.Do(req)
	if err == nil {
		defer func() { _ = resp.Body.Close() }()
		// 任何 HTTP 应答(含 401)都证明服务在监听;401/403 说明 token 不匹配。
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return &TestResult{OK: false, LatencyMs: time.Since(start).Milliseconds(),
				Detail: fmt.Sprintf("服务在监听但拒绝了访问(HTTP %d):令牌不匹配或已启用鉴权", resp.StatusCode)}
		}
		return &TestResult{OK: true, LatencyMs: time.Since(start).Milliseconds(),
			Detail: fmt.Sprintf("Trivy 服务健康检查通过(HTTP %d)", resp.StatusCode)}
	}
	// /healthz 不可达(老版本无该端点)→ 回落 TCP 探活,只验证端口可连。
	dctx, dcancel := context.WithTimeout(ctx, 5*time.Second)
	defer dcancel()
	conn, derr := net.DialTimeout("tcp", net.JoinHostPort(host, fmt.Sprint(port)), 5*time.Second)
	_ = dctx
	if derr != nil {
		return &TestResult{Err: derr, Detail: "无法连接:" + humanProbeError(derr)}
	}
	_ = conn.Close()
	return &TestResult{OK: true, LatencyMs: time.Since(start).Milliseconds(),
		Detail: "端口可连(服务版本较老,无 /healthz 端点;按 TCP 探活判定)"}
}

// humanProbeError 把拨号错误转人话(不含内部细节)。
func humanProbeError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "timeout") || strings.Contains(msg, "i/o timeout"):
		return "连接超时(端口未开放或主机不可达)"
	case strings.Contains(msg, "refused"):
		return "连接被拒绝(端口无服务监听)"
	case strings.Contains(msg, "no such host"):
		return "主机名解析失败"
	default:
		return "主机不可达或端口未开放"
	}
}
