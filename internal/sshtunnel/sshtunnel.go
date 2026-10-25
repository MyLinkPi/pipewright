// Package sshtunnel 是「SSH 隧道」领域层:配置管理 + 独立 helper 子进程运行时。
//
// 三种隧道语义(单隧道单转发,等价 ssh -N 单参数):
//   - local(-L)  :本机监听 bind,连接经远端 sshd 转发到 dest(远端侧可达地址);
//   - remote(-R) :远端 sshd 监听 bind(需 GatewayPorts 才能 0.0.0.0),连接回传后
//                  在本机连 dest(本机侧可达地址);
//   - dynamic(-D):本机监听 bind 充当 SOCKS5 入口(无认证、仅 CONNECT),目标地址
//                  经远端 sshd 解析并连接(域名由远端解析,同 ssh -D)。
//
// SSH 目标二选一:绑定已登记服务器(server_id,沿用其跳板链)或直连任意主机
// (host/port/user + vault 凭据引用,支持不在 servers 表里的机器)。
//
// 运行模型(重启 pipewright 不影响隧道,对齐 mesh easytier 的收养机制):
//   - 每条隧道一个 helper 子进程(同一二进制以 --tunnel-helper --id <id> re-exec 自己),
//     与主进程无父子依赖:宿主停机/自升级**不杀** helper,helper 成孤儿继续服务;
//   - helper 自带断线重连(退避 2s→30s)+ 每 5s 心跳(keepalive + 状态写
//     <dataDir>/tunnels/state/<id>.json,原子替换)——宿主不在期间网络闪断也能自愈;
//   - 主进程 10s 对账循环:心跳新鲜 → 沿用(零中断收养);无心跳 → 重拉;停用 → 停止;
//     配置指纹变化 → 重启。helper 的 argv 只有隧道 id,绝无密钥(AC-SEC-02);
//     凭据由 helper 自行经 vault 解析,明文仅进程内即用即弃。
//
// 凭据引用只入库 id(AC-SEC-01);连接错误经 target 哨兵映射为人读文案,绝无明文。
package sshtunnel

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/huangchengsir/pipewright/internal/target"
)

// Mode 枚举(入库白名单)。
const (
	ModeLocal   = "local"   // ssh -L
	ModeRemote  = "remote"  // ssh -R
	ModeDynamic = "dynamic" // ssh -D
	// ModeVPN 是 ssh -w 的点对点 tun 隧道(L3 VPN)。不推荐:仅 Linux 控制节点
	// (需 root/CAP_NET_ADMIN),目标机 sshd 须开启 PermitTunnel(默认 no),地址
	// 需手工规划 —— 一般场景请用平台内置的集群自组网(EasyTier)。字段语义换轨:
	//   bind_host = 本端隧道 IP、dest_host = 远端隧道 IP(须同一 /30);
	//   bind_port = 本端 tun 序号、dest_port = 远端 tun 序号(0-255)。
	ModeVPN = "vpn"
)

// 心跳/对账参数(helper 与主进程共享的协议常量)。
const (
	// heartbeatInterval 是 helper 心跳节拍(状态文件 updatedAt 的写入频率)。
	heartbeatInterval = 5 * time.Second
	// staleAfter 是心跳视为失效的阈值(连续 4 拍未更新 = helper 已死)。
	staleAfter = 20 * time.Second
	// reconcileInterval 是主进程对账节拍(拉起/停止/重启决策)。
	reconcileInterval = 10 * time.Second
)

// 领域错误(错误体绝无凭据明文/内部栈)。
var (
	// ErrInvalidInput 配置非法(缺名/模式/端口/目标二选一冲突等)。HTTP 层映射 400。
	ErrInvalidInput = errors.New("sshtunnel: invalid input")
	// ErrNotFound 隧道不存在。HTTP 层映射 404。
	ErrNotFound = errors.New("sshtunnel: tunnel not found")
	// ErrUninitialized 服务未装配。HTTP 层映射 503。
	ErrUninitialized = errors.New("sshtunnel: service uninitialized")
	// ErrBindConflict 与另一条已启用的本机监听隧道 bind 冲突。HTTP 层映射 409。
	ErrBindConflict = errors.New("sshtunnel: bind address conflict")
)

// Tunnel 是隧道配置模型(绝无凭据明文,只持引用)。
type Tunnel struct {
	ID   string
	Name string
	// ServerID 绑定已登记服务器(连接沿用其 host/port/user/凭据/跳板链);空 = 直连模式。
	ServerID string
	// Host/Port/User/CredentialID 是直连模式字段(目标机不在 servers 表;无跳板链)。
	Port         int
	Host         string
	User         string
	CredentialID string
	// Mode 是隧道模式(ModeLocal | ModeRemote | ModeDynamic | ModeVPN)。
	Mode string
	// BindHost/BindPort 是监听地址(local/dynamic = 本机;remote = 远端 sshd 侧);
	// BindPort 0 = 自动分配,实际端口经心跳文件回显。vpn 模式下语义换轨:
	// bind_host = 本端隧道 IP、bind_port = 本端 tun 序号(见 ModeVPN)。
	BindHost string
	BindPort int
	// DestHost/DestPort 是转发目标(local = 远端侧可达;remote = 本机侧可达;
	// dynamic 留空;vpn = 远端隧道 IP / 远端 tun 序号)。
	DestHost string
	DestPort int
	// Enabled 兼作 autostart 与期望状态:false = 停用并停进程。
	Enabled bool
	// ServerName/CredentialName 是 join 冗余展示名;非持久列。
	ServerName     string
	CredentialName string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Fingerprint 是影响连接/转发的配置指纹(变更即重启 helper;enabled/name 不参与)。
func (t *Tunnel) Fingerprint() string {
	parts := []string{
		t.ServerID, t.Host, strconv.Itoa(t.Port), t.User, t.CredentialID,
		t.Mode, t.BindHost, strconv.Itoa(t.BindPort), t.DestHost, strconv.Itoa(t.DestPort),
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return hex.EncodeToString(sum[:8])
}

// Input 是创建/更新入参(全量替换;HTTP 层解码后传入)。
type Input struct {
	Name         string
	ServerID     string
	Host         string
	Port         int
	User         string
	CredentialID string
	Mode         string
	BindHost     string
	BindPort     int
	DestHost     string
	DestPort     int
	Enabled      bool
}

// TestResult 是隧道连通性测试结果(一次性;不驻留进程)。
type TestResult struct {
	OK        bool
	LatencyMs int64
	Err       string
}

// Service 组合隧道 CRUD、helper 运行时与连通性测试。
type Service struct {
	db      *sql.DB
	targets target.Service
	rt      *Runtime
}

// New 构造 Service。db/targets 由装配方保证非 nil(缺失时 CRUD 返回 ErrUninitialized);
// dataDir 是数据目录(状态/日志落 <dataDir>/tunnels/)。
func New(db *sql.DB, targets target.Service, dataDir string) *Service {
	return &Service{db: db, targets: targets, rt: NewRuntime(dataDir)}
}

// List 返回全部隧道(按创建序),合并 helper 运行态(心跳新鲜才计为在运行)。
func (s *Service) List(ctx context.Context) ([]TunnelView, error) {
	tunnels, err := s.list(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]TunnelView, 0, len(tunnels))
	for _, t := range tunnels {
		out = append(out, s.view(t))
	}
	return out, nil
}

// view 把配置与运行态合为视图:心跳新鲜 → helper 汇报的 connecting/active/retrying;
// 否则 enabled → stopped(对账循环会自动拉起)、disabled → disabled。
func (s *Service) view(t *Tunnel) TunnelView {
	v := TunnelView{Tunnel: *t, Status: StatusDisabled}
	st := s.rt.State(t.ID)
	if st != nil && st.Live() {
		v.RuntimeStatus = st.Status
		v.PID = st.PID
		v.LastError = st.LastError
		v.Restarts = st.Restarts
		v.ActualPort = st.ActualPort
		v.Endpoint = st.Endpoint
		v.StartedAt = st.StartedAtTime()
		if t.Enabled {
			v.Status = st.Status
		}
	} else if t.Enabled {
		v.Status = StatusStopped
	}
	return v
}

// Create 校验并入库;enabled 时立即拉起 helper(不等对账节拍)。
func (s *Service) Create(ctx context.Context, in Input) (*TunnelView, error) {
	if s == nil || s.db == nil {
		return nil, ErrUninitialized
	}
	t, err := s.normalizeAndValidate(ctx, in, "")
	if err != nil {
		return nil, err
	}
	t.ID = uuid.NewString()
	nowStr := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO ssh_tunnels (id, name, server_id, host, port, user, credential_id, mode, bind_host, bind_port, dest_host, dest_port, enabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Name, nullString(t.ServerID), t.Host, t.Port, t.User, nullString(t.CredentialID),
		t.Mode, t.BindHost, t.BindPort, t.DestHost, t.DestPort, boolToInt(t.Enabled), nowStr, nowStr,
	); err != nil {
		// FK RESTRICT(引用的 server/凭据已不存在)映射为人读错误。
		if isForeignKeyErr(err) {
			return nil, target.ErrCredentialNotFound
		}
		return nil, fmt.Errorf("sshtunnel: create tunnel: %w", err)
	}
	created, err := s.load(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	if created.Enabled {
		s.spawnLogged(created)
	}
	view := s.view(created)
	return &view, nil
}

// Update 全量替换配置;运行中且指纹变化 → 重启 helper;停用 → 停进程;启用未跑 → 拉起。
func (s *Service) Update(ctx context.Context, id string, in Input) (*TunnelView, error) {
	if s == nil || s.db == nil {
		return nil, ErrUninitialized
	}
	if _, err := s.load(ctx, id); err != nil {
		return nil, err
	}
	t, err := s.normalizeAndValidate(ctx, in, id)
	if err != nil {
		return nil, err
	}
	t.ID = id
	nowStr := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE ssh_tunnels SET name = ?, server_id = ?, host = ?, port = ?, user = ?, credential_id = ?,
		 mode = ?, bind_host = ?, bind_port = ?, dest_host = ?, dest_port = ?, enabled = ?, updated_at = ?
		 WHERE id = ?`,
		t.Name, nullString(t.ServerID), t.Host, t.Port, t.User, nullString(t.CredentialID),
		t.Mode, t.BindHost, t.BindPort, t.DestHost, t.DestPort, boolToInt(t.Enabled), nowStr, id,
	); err != nil {
		if isForeignKeyErr(err) {
			return nil, target.ErrCredentialNotFound
		}
		return nil, fmt.Errorf("sshtunnel: update tunnel: %w", err)
	}
	updated, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	s.applyDesiredState(updated)
	view := s.view(updated)
	return &view, nil
}

// applyDesiredState 把「期望状态」对齐到运行时:停用即停;启用未跑即拉;
// 运行中配置指纹变化即重启(与对账循环同一决策,操作后立即生效不等节拍)。
func (s *Service) applyDesiredState(t *Tunnel) {
	st := s.rt.State(t.ID)
	live := st != nil && st.Live()
	switch {
	case !t.Enabled:
		if live {
			s.rt.Stop(t.ID)
		}
	case !live:
		s.spawnLogged(t)
	case st.Fingerprint != t.Fingerprint():
		s.rt.Stop(t.ID)
		s.spawnLogged(t)
	}
}

// spawnLogged 拉起 helper,失败仅记日志且同文本去重(配置已入库,对账循环会持续重试)。
func (s *Service) spawnLogged(t *Tunnel) {
	if err := s.rt.Spawn(t.ID); err != nil {
		if s.rt.ShouldLogSpawnFail(t.ID, err.Error()) {
			log.Printf("[sshtunnel] 警告:隧道 %s helper 拉起失败(对账循环将重试):%v", t.Name, err)
		}
	}
}

// Delete 停止 helper、删配置行并清理状态/日志文件。
func (s *Service) Delete(ctx context.Context, id string) error {
	if s == nil || s.db == nil {
		return ErrUninitialized
	}
	if _, err := s.load(ctx, id); err != nil {
		return err
	}
	s.rt.Stop(id)
	if _, err := s.db.ExecContext(ctx, `DELETE FROM ssh_tunnels WHERE id = ?`, id); err != nil {
		return fmt.Errorf("sshtunnel: delete tunnel: %w", err)
	}
	s.rt.Cleanup(id)
	return nil
}

// Start 启用并拉起(已在跑则重启,保证读到最新配置)。
func (s *Service) Start(ctx context.Context, id string) (*TunnelView, error) {
	return s.setEnabled(ctx, id, true)
}

// Stop 停止进程并停用(enabled = false)。
func (s *Service) Stop(ctx context.Context, id string) (*TunnelView, error) {
	return s.setEnabled(ctx, id, false)
}

// setEnabled 落库期望状态并立即对齐运行时。
func (s *Service) setEnabled(ctx context.Context, id string, enabled bool) (*TunnelView, error) {
	if s == nil || s.db == nil {
		return nil, ErrUninitialized
	}
	if _, err := s.load(ctx, id); err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE ssh_tunnels SET enabled = ?, updated_at = ? WHERE id = ?`,
		boolToInt(enabled), time.Now().UTC().Format(time.RFC3339), id); err != nil {
		return nil, fmt.Errorf("sshtunnel: set tunnel enabled: %w", err)
	}
	t, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	s.applyDesiredState(t)
	view := s.view(t)
	return &view, nil
}

// Restart 重启 helper(不改变 enabled;停用状态下等价于 Start)。
func (s *Service) Restart(ctx context.Context, id string) (*TunnelView, error) {
	if s == nil || s.db == nil {
		return nil, ErrUninitialized
	}
	if _, err := s.load(ctx, id); err != nil {
		return nil, err
	}
	s.rt.Stop(id)
	t, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	if t.Enabled {
		s.spawnLogged(t)
	}
	view := s.view(t)
	return &view, nil
}

// Test 一次性连通性验证:按连接方式建连(remote 加试远端监听,local/dynamic 试本机
// 监听可用性),不驻留进程。错误为人读文案,绝无凭据明文。
func (s *Service) Test(ctx context.Context, id string) (*TestResult, error) {
	if s == nil || s.db == nil || s.targets == nil {
		return nil, ErrUninitialized
	}
	t, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	res := &TestResult{}
	client, err := DialTunnel(ctx, t, s.targets)
	res.LatencyMs = time.Since(start).Milliseconds()
	if err != nil {
		res.Err = humanError(err)
		return res, nil
	}
	defer client.Close()
	res.LatencyMs = time.Since(start).Milliseconds()

	// vpn(-w):只验证远端接受 tun@openssh.com 请求(PermitTunnel + 序号可用)即关;
	// 本机 tun 建立需 root,不在此触碰。
	if t.Mode == ModeVPN {
		ch, terr := client.OpenTunChannel(t.BindPort, t.DestPort)
		if terr != nil {
			res.Err = humanListenError(terr)
			return res, nil
		}
		_ = ch.Close()
		res.OK = true
		return res, nil
	}

	// 转发面可用性:remote 试远端监听即关;local/dynamic 试本机监听即关
	//(bind_port=0 时由内核临时分配,仅验证可监听)。
	ln, lerr := listenForwardSide(client, t)
	if lerr != nil {
		res.Err = humanListenError(lerr)
		return res, nil
	}
	_ = ln.Close()
	res.OK = true
	return res, nil
}

// Logs 返回 helper 日志尾部 lines 行(默认 200,钳到 1000;从未运行过 → 空列表)。
func (s *Service) Logs(id string, lines int) ([]string, error) {
	if s == nil {
		return nil, ErrUninitialized
	}
	if lines <= 0 {
		lines = 200
	}
	if lines > 1000 {
		lines = 1000
	}
	return s.rt.TailLog(id, lines), nil
}

// StartLoop 启动对账循环(随 ctx 启停;立即跑一拍再按节拍轮询)。
// 停机语义同 mesh:ctx 取消直接返回,**不杀**任何 helper(孤儿继续服务,
// 下次启动心跳新鲜即收养)。
func (s *Service) StartLoop(ctx context.Context) {
	s.reconcile(ctx)
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reconcile(ctx)
		}
	}
}

// reconcile 对齐「配置期望」与「运行中的 helper」:停用即停;启用无活 helper 即拉
//(含陈旧状态文件清理);运行中指纹变化即重启。单拍内逐隧道决策,互不阻断。
func (s *Service) reconcile(ctx context.Context) {
	tunnels, err := s.list(ctx)
	if err != nil {
		log.Printf("[sshtunnel] 对账读取配置失败:%v", err)
		return
	}
	for _, t := range tunnels {
		st := s.rt.State(t.ID)
		live := st != nil && st.Live()
		switch {
		case !t.Enabled:
			if live {
				log.Printf("[sshtunnel] 隧道 %s 已停用,停止 helper", t.Name)
				s.rt.Stop(t.ID)
			} else if st != nil {
				s.rt.CleanupState(t.ID)
			}
		case !live:
			if st != nil {
				// 心跳过期 = helper 已死但没来得及清理,先回收状态文件再重拉。
				s.rt.CleanupState(t.ID)
			}
			s.spawnLogged(t)
		case st.Fingerprint != t.Fingerprint():
			log.Printf("[sshtunnel] 隧道 %s 配置已变更,重启 helper", t.Name)
			s.rt.Stop(t.ID)
			s.spawnLogged(t)
		}
	}
}

// DialTunnel 按隧道的连接方式建立长连接:绑服务器 → target.Dial(含跳板链);
// 直连 → target.DialDirect。
func DialTunnel(ctx context.Context, t *Tunnel, targets target.Service) (target.SSHClient, error) {
	if t.ServerID != "" {
		return targets.Dial(ctx, t.ServerID)
	}
	return targets.DialDirect(ctx, t.Host, t.Port, t.User, t.CredentialID)
}

// --- 配置校验 ---

// normalizeAndValidate 归一并校验入参(excludeID 用于 bind 冲突检查时排除自身)。
func (s *Service) normalizeAndValidate(ctx context.Context, in Input, excludeID string) (*Tunnel, error) {
	t := &Tunnel{
		Name:         strings.TrimSpace(in.Name),
		ServerID:     strings.TrimSpace(in.ServerID),
		Host:         strings.TrimSpace(in.Host),
		User:         strings.TrimSpace(in.User),
		CredentialID: strings.TrimSpace(in.CredentialID),
		Mode:         strings.TrimSpace(in.Mode),
		BindHost:     strings.TrimSpace(in.BindHost),
		BindPort:     in.BindPort,
		DestHost:     strings.TrimSpace(in.DestHost),
		DestPort:     in.DestPort,
		Enabled:      in.Enabled,
		Port:         in.Port,
	}
	if t.Name == "" {
		return nil, ErrInvalidInput
	}
	switch t.Mode {
	case ModeLocal, ModeRemote, ModeDynamic, ModeVPN:
	default:
		return nil, ErrInvalidInput
	}
	// 连接方式互斥:绑服务器则直连字段必须全空;直连则 host/user/凭据必填。
	if t.ServerID != "" {
		if t.Host != "" || t.User != "" || t.CredentialID != "" {
			return nil, ErrInvalidInput
		}
		if s.targets == nil {
			return nil, ErrUninitialized
		}
		if _, err := s.targets.Get(ctx, t.ServerID); err != nil {
			return nil, err // ErrNotFound 等,httpapi 映射
		}
	} else {
		if t.Host == "" || t.User == "" || t.CredentialID == "" {
			return nil, ErrInvalidInput
		}
		if t.Port <= 0 {
			t.Port = target.DefaultPort
		}
		if t.Port < 1 || t.Port > 65535 {
			return nil, ErrInvalidInput
		}
		if err := s.validateCredentialType(ctx, t.CredentialID); err != nil {
			return nil, err
		}
	}
	if t.BindHost == "" && t.Mode != ModeVPN {
		t.BindHost = "127.0.0.1" // 安全默认:本机回环(防 -D 代理裸奔;vpn 模式必须显式给本端隧道 IP)
	}
	if t.BindPort < 0 || t.BindPort > 65535 {
		return nil, ErrInvalidInput
	}
	switch t.Mode {
	case ModeDynamic:
		if t.DestHost != "" || t.DestPort != 0 {
			return nil, ErrInvalidInput
		}
	case ModeVPN:
		// vpn(-w):两端隧道 IP 须为合法 IPv4 且同一 /30;tun 序号 0-255。
		localIP := net.ParseIP(t.BindHost)
		remoteIP := net.ParseIP(t.DestHost)
		if localIP == nil || localIP.To4() == nil || remoteIP == nil || remoteIP.To4() == nil {
			return nil, ErrInvalidInput
		}
		mask := net.CIDRMask(30, 32)
		if !localIP.To4().Mask(mask).Equal(remoteIP.To4().Mask(mask)) {
			return nil, ErrInvalidInput
		}
		if t.BindPort < 0 || t.BindPort > 255 || t.DestPort < 0 || t.DestPort > 255 {
			return nil, ErrInvalidInput
		}
	default: // local | remote
		if t.DestHost == "" || t.DestPort < 1 || t.DestPort > 65535 {
			return nil, ErrInvalidInput
		}
	}
	if err := s.checkBindConflict(ctx, t, excludeID); err != nil {
		return nil, err
	}
	return t, nil
}

// validateCredentialType 校验直连凭据存在且类型为 ssh_key / ssh_password(防把
// git token 等明文误当 SSH 认证材料;与 target.validateJumpCredentials 同一守卫)。
func (s *Service) validateCredentialType(ctx context.Context, credentialID string) error {
	var typ string
	err := s.db.QueryRowContext(ctx, `SELECT type FROM credentials WHERE id = ?`, credentialID).Scan(&typ)
	if errors.Is(err, sql.ErrNoRows) {
		return target.ErrCredentialNotFound
	}
	if err != nil {
		return fmt.Errorf("sshtunnel: check credential type: %w", err)
	}
	if typ != "ssh_key" && typ != "ssh_password" {
		return target.ErrCredentialTypeMismatch
	}
	return nil
}

// checkBindConflict 检查本机监听(local/dynamic)端口冲突:另一条已启用的同 bind
// 隧道会真实抢端口,提前拒绝给出清晰错误(remote 的 bind 在远端 sshd 侧,不在此列;
// 新隧道自身 enabled 与否都查 —— Start 路径不复查,保存时就得拦)。
func (s *Service) checkBindConflict(ctx context.Context, t *Tunnel, excludeID string) error {
	if t.Mode == ModeRemote || t.BindPort == 0 {
		return nil
	}
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ssh_tunnels
		 WHERE enabled = 1 AND bind_port = ? AND bind_host = ? AND mode IN ('local', 'dynamic') AND id != ?`,
		t.BindPort, t.BindHost, excludeID,
	).Scan(&count)
	if err != nil {
		return fmt.Errorf("sshtunnel: check bind conflict: %w", err)
	}
	if count > 0 {
		return ErrBindConflict
	}
	return nil
}

// --- 存取 ---

// list 全量加载(join servers/credentials 取展示名;按创建序)。
func (s *Service) list(ctx context.Context) ([]*Tunnel, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT t.id, t.name, t.server_id, t.host, t.port, t.user, t.credential_id, t.mode,
		        t.bind_host, t.bind_port, t.dest_host, t.dest_port, t.enabled, t.created_at, t.updated_at,
		        COALESCE(s.name, ''), COALESCE(c.name, '')
		 FROM ssh_tunnels t
		 LEFT JOIN servers s ON s.id = t.server_id
		 LEFT JOIN credentials c ON c.id = t.credential_id
		 ORDER BY t.created_at ASC, t.id ASC`)
	if err != nil {
		return nil, fmt.Errorf("sshtunnel: list tunnels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*Tunnel
	for rows.Next() {
		t, err := scanTunnel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// load 按 id 加载(不含展示名;校验/操作路径用)。
func (s *Service) load(ctx context.Context, id string) (*Tunnel, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT t.id, t.name, t.server_id, t.host, t.port, t.user, t.credential_id, t.mode,
		        t.bind_host, t.bind_port, t.dest_host, t.dest_port, t.enabled, t.created_at, t.updated_at,
		        COALESCE(s.name, ''), COALESCE(c.name, '')
		 FROM ssh_tunnels t
		 LEFT JOIN servers s ON s.id = t.server_id
		 LEFT JOIN credentials c ON c.id = t.credential_id
		 WHERE t.id = ?`, id)
	return scanTunnel(row)
}

// LoadByID 是 helper 子进程的取配置入口(导出供 cmd 装配复用)。
func LoadByID(ctx context.Context, db *sql.DB, id string) (*Tunnel, error) {
	s := &Service{db: db}
	return s.load(ctx, id)
}

// scanner 抽象 *sql.Row 与 *sql.Rows 的 Scan。
type scanner interface {
	Scan(dest ...any) error
}

func scanTunnel(sc scanner) (*Tunnel, error) {
	var (
		t                Tunnel
		serverID, credID sql.NullString
		enabled          int
		created, updated string
	)
	if err := sc.Scan(
		&t.ID, &t.Name, &serverID, &t.Host, &t.Port, &t.User, &credID, &t.Mode,
		&t.BindHost, &t.BindPort, &t.DestHost, &t.DestPort, &enabled, &created, &updated,
		&t.ServerName, &t.CredentialName,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("sshtunnel: scan tunnel: %w", err)
	}
	t.ServerID = serverID.String
	t.CredentialID = credID.String
	t.Enabled = enabled != 0
	var err error
	if t.CreatedAt, err = time.Parse(time.RFC3339, created); err != nil {
		t.CreatedAt = time.Time{}
	}
	if t.UpdatedAt, err = time.Parse(time.RFC3339, updated); err != nil {
		t.UpdatedAt = time.Time{}
	}
	return &t, nil
}

// nullString 空串 → NULL(server_id/credential_id 的可空外键形态)。
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// isForeignKeyErr 判定外键约束失败(SQLite/MySQL 文本匹配;与 target 包同款)。
func isForeignKeyErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToUpper(err.Error())
	return strings.Contains(msg, "FOREIGN KEY") || strings.Contains(msg, "CONSTRAINT")
}

// humanError 把 target 领域错误映射为人读文案(绝无凭据明文/内部栈)。
func humanError(err error) string {
	switch {
	case errors.Is(err, target.ErrAuth):
		return "SSH 认证失败:密钥或口令无效,或无登录权限"
	case errors.Is(err, target.ErrUnreachable):
		return "无法连接目标主机:端口未开放、主机不可达或超时"
	case errors.Is(err, target.ErrInvalidCredential):
		return "凭据不是可用的 SSH 私钥或口令"
	case errors.Is(err, target.ErrCredentialNotFound):
		return "引用的凭据不存在"
	case errors.Is(err, target.ErrVaultUnconfigured):
		return "保险库未配置 master key,无法取 SSH 凭据"
	case errors.Is(err, target.ErrNotFound):
		return "绑定的服务器不存在或已删除"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "连接超时"
	default:
		return "连接失败"
	}
}

// humanListenError 把监听失败映射为人读文案(含 remote 模式的 GatewayPorts、
// vpn 模式的 PermitTunnel 提示)。
func humanListenError(err error) string {
	msg := err.Error()
	if strings.Contains(msg, "PermitTunnel") {
		return "远端拒绝 tun 转发:需在目标机 sshd 配置 PermitTunnel yes(且该 tun 序号未被占用)"
	}
	if strings.Contains(msg, "GatewayPorts") || strings.Contains(msg, " administratively prohibited") {
		return "远端监听被拒绝:绑定 0.0.0.0 需在目标机 sshd 开启 GatewayPorts"
	}
	return "监听失败:" + msg
}

// listenForwardSide 按模式试开监听即关(remote → 远端 sshd;local/dynamic → 本机)。
// 只做可用性验证,返回的 listener 由调用方立即关闭。
func listenForwardSide(client target.SSHClient, t *Tunnel) (net.Listener, error) {
	addr := net.JoinHostPort(t.BindHost, strconv.Itoa(t.BindPort))
	if t.Mode == ModeRemote {
		return client.Listen("tcp", addr)
	}
	return net.Listen("tcp", addr)
}
