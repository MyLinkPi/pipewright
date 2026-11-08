package target

// 本机(控制节点)执行支持。
//
// 「本机」= 运行 Pipewright 的这台主机(控制节点)。它**不是**登记服务器:不进 servers 表、
// 不可创建/改/删,但作为容器域(容器/镜像/栈/卷/网络/日志/终端)的一等目标,命令直接在本机
// 执行(os/exec,**不经 SSH**),从而复用全部既有容器 handler 与它们的目标机零侵入纪律。
//
// 用法:装配路由时用 WithLocalHost(base) 包装 target.Service 再注入容器域 handler。
//   - 对真实 serverID:逐字透传(零行为变化)。
//   - 对 LocalHostID:Get 返回合成 Server(供 handler 的定位校验),Exec/ExecStream/
//     ExecInteractive/Upload/ExecWithStdin 走本地执行。
//
// 角色(mode,specs/local-node-roles.md):WithLocalHostRoles(base, rolesFn) 在此之上叠加
// 「角色开关」——deploy/servicereg(网关)/调度器各持独立实例,rolesFn 只回答「本角色是否启用」:
//   - 启用:语义与旧包装一致(Get 合成 / 执行类走本机);List 可按需追加合成 Server
//     (部署标签圈选消费)。
//   - 禁用:对 __local__ 的一切执行类请求**显式**返回 ErrLocalRoleDisabled(→ 422
//     local_role_disabled),绝不静默落到本机执行 —— 这是「每角色独立包装」的安全底线。
//
// 安全(AC-SEC-02):命令仍以 []string(程序 + 参数)传入,本地 `exec.Command` 直接用它,
// 不经 shell → 无注入面;无凭据参与(本机无需 SSH 认证),也就无凭据泄漏面。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// LocalHostID 是控制节点的伪 serverID。与前端 `api/servers.ts` 的 `LOCAL_HOST_ID` 以及
// 本机状态卡牌的 `serverId` 对齐,三处必须是同一个字面量。
const LocalHostID = "__local__"

var (
	// ErrLocalHostImmutable 表示试图把控制节点当作可登记服务器改写/删除(不允许)。
	ErrLocalHostImmutable = errors.New("target: local control node is not a registered server")
	// ErrEmptyCommand 表示本机命令 array 为空。
	ErrEmptyCommand = errors.New("target: empty command")
	// ErrLocalInteractiveUnsupported 表示当前平台不支持本机 PTY 交互终端(仅类 Unix)。
	ErrLocalInteractiveUnsupported = errors.New("target: local interactive session unsupported on this platform")
	// ErrLocalRoleDisabled 表示本机角色未启用:控制节点对该角色的哨兵请求(构建机池/
	// 部署目标/网关主机)被显式拒绝,决不静默落到本机执行。httpapi 映射 422
	// local_role_disabled(人读:本机对应角色未启用)。
	ErrLocalRoleDisabled = errors.New("target: local node role disabled")
)

// localExitNotFound / localExitNotExecutable 是本机命令「无法启动」时回给调用方的伪退出码:
// 127 = 命令不存在(与 POSIX shell 一致),126 = 存在但不可执行/无权限。**刻意不返回 error** ——
// 本机不存在「连不上」这回事,非零退出码足以让上层(如容器运行时探测)走「该运行时不可用」
// 的正常降级分支,而不是被误判为「不可达」。
const (
	localExitNotFound      = 127
	localExitNotExecutable = 126
)

// LocalRoles 是 WithLocalHostRoles 的角色开关快照(每次调用重新求值,开关热切换即生效;
// 由装配层从 localnode.Service 读取,本包不触库)。
type LocalRoles struct {
	// Enabled 报告该角色是否启用。禁用时对 __local__ 的执行类请求显式返回 ErrLocalRoleDisabled。
	Enabled bool
	// InList 报告本机是否出现在 List() 尾部(部署标签圈选消费;网关/调度器包装恒 false)。
	InList bool
	// Labels / MaxBuilds / Priority 是构建机池参数(与 servers 同语义;填充进合成 Server)。
	Labels    string
	MaxBuilds int
	Priority  int
}

// RolesFn 惰性求值角色开关。nil = 旧 WithLocalHost 语义(恒启用,不追加 List)。
type RolesFn func() LocalRoles

// WithLocalHost 返回包装后的 Service:serverID == LocalHostID 时改用本机执行,其余一律透传。
// base 为 nil 时原样返回 nil(保持既有「服务未初始化」的 503 语义)。签名与行为冻结不变。
func WithLocalHost(base Service) Service {
	if base == nil {
		return nil
	}
	return &localHostService{Service: base}
}

// WithLocalHostRoles 在 WithLocalHost 之上叠加角色开关(见文件头「角色」段)。rolesFn 为 nil
// 时与 WithLocalHost 完全一致。真实 serverID 逐字透传的性质不变 —— 关闭态零行为变化。
func WithLocalHostRoles(base Service, rolesFn RolesFn) Service {
	if base == nil {
		return nil
	}
	return &localHostService{Service: base, roles: rolesFn}
}

// localHostService 是 Service 的装饰器。未覆盖的方法(Dial/DialDirect 等)经嵌入
// 接口逐字委托给 base;对 __local__ 它们落到 base 的库查询 → ErrNotFound(本机本就不在
// servers 表,隧道等共享面从不引用该 id,与既有语义一致)。
type localHostService struct {
	Service
	roles RolesFn
}

// rolesEnabled 报告当前角色是否启用:未装配 rolesFn(旧 WithLocalHost)恒启用。
func (s *localHostService) rolesEnabled() bool {
	if s.roles == nil {
		return true
	}
	return s.roles().Enabled
}

// localRoles 求值当前角色快照(rolesFn 为 nil 时返回零值)。
func (s *localHostService) localRoles() LocalRoles {
	if s.roles == nil {
		return LocalRoles{}
	}
	return s.roles()
}

// localHostServer 返回控制节点的合成 Server 视图(无凭据字段;不入库、只读)。
func localHostServer() *Server {
	return &Server{
		ID:   LocalHostID,
		Name: "本机(控制节点)",
		Host: "localhost",
		Port: 0,
		User: localUserName(),
	}
}

// localServer 返回带角色参数的合成 Server(labels/槽位/优先级随开关实时)。
func (s *localHostService) localServer() *Server {
	srv := localHostServer()
	r := s.localRoles()
	srv.Labels = r.Labels
	srv.MaxBuilds = r.MaxBuilds
	srv.Priority = r.Priority
	return srv
}

// localUserName 尽力取本机当前用户名(仅展示用);取不到回退环境变量/占位串。
func localUserName() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if v := os.Getenv("USER"); v != "" {
		return v
	}
	if v := os.Getenv("USERNAME"); v != "" {
		return v
	}
	return "local"
}

// Get 对控制节点返回合成 Server(供 handler 的存在性校验);角色禁用 → ErrLocalRoleDisabled;
// 其余透传。
func (s *localHostService) Get(ctx context.Context, id string) (*Server, error) {
	if id != LocalHostID {
		return s.Service.Get(ctx, id)
	}
	if !s.rolesEnabled() {
		return nil, ErrLocalRoleDisabled
	}
	return s.localServer(), nil
}

// List 追加控制节点合成 Server(部署标签圈选消费;InList 未开或角色禁用时不追加,
// 零命中语义不变);旧 WithLocalHost(roles=nil)逐字透传。其余透传。
func (s *localHostService) List(ctx context.Context) ([]*Server, error) {
	list, err := s.Service.List(ctx)
	if err != nil {
		return nil, err
	}
	if s.roles == nil {
		return list, nil
	}
	r := s.roles()
	if !r.Enabled || !r.InList {
		return list, nil
	}
	srv := localHostServer()
	srv.Labels = r.Labels
	srv.MaxBuilds = r.MaxBuilds
	srv.Priority = r.Priority
	return append(list, srv), nil
}

// Update 拒绝改写控制节点(它不是登记服务器;与角色开关无关,不变量保留)。
func (s *localHostService) Update(ctx context.Context, id string, in UpdateInput) (*Server, error) {
	if id == LocalHostID {
		return nil, ErrLocalHostImmutable
	}
	return s.Service.Update(ctx, id, in)
}

// Delete 拒绝删除控制节点。
func (s *localHostService) Delete(ctx context.Context, id string) error {
	if id == LocalHostID {
		return ErrLocalHostImmutable
	}
	return s.Service.Delete(ctx, id)
}

// Test 对控制节点直接返回本地运行时信息(本机恒可达,无需跑 platform probe 命令 ——
// 那些命令是给远端 Linux 目标用的,在 Windows 控制节点上并不存在);其余透传。
func (s *localHostService) Test(ctx context.Context, id string) (*TestResult, error) {
	if id != LocalHostID {
		return s.Service.Test(ctx, id)
	}
	if !s.rolesEnabled() {
		return nil, ErrLocalRoleDisabled
	}
	host, _ := os.Hostname()
	return &TestResult{
		OK:     true,
		Output: strings.TrimSpace(runtime.GOOS + "/" + runtime.GOARCH + " " + host),
	}, nil
}

// Exec 对控制节点在本机执行 cmd;其余经 SSH 透传。
func (s *localHostService) Exec(ctx context.Context, serverID string, cmd []string) (*ExecResult, error) {
	if serverID != LocalHostID {
		return s.Service.Exec(ctx, serverID, cmd)
	}
	if !s.rolesEnabled() {
		return nil, ErrLocalRoleDisabled
	}
	return runLocalCommand(ctx, cmd, nil)
}

// ExecWithStdin 同 Exec,但把 stdin 接到本机命令的标准输入;其余透传。
func (s *localHostService) ExecWithStdin(ctx context.Context, serverID string, cmd []string, stdin io.Reader) (*ExecResult, error) {
	if serverID != LocalHostID {
		return s.Service.ExecWithStdin(ctx, serverID, cmd, stdin)
	}
	if !s.rolesEnabled() {
		return nil, ErrLocalRoleDisabled
	}
	return runLocalCommand(ctx, cmd, stdin)
}

// ExecStream 对控制节点返回本机命令 stdout 的流(实时日志);其余透传。
func (s *localHostService) ExecStream(ctx context.Context, serverID string, cmd []string) (io.ReadCloser, error) {
	if serverID != LocalHostID {
		return s.Service.ExecStream(ctx, serverID, cmd)
	}
	if !s.rolesEnabled() {
		return nil, ErrLocalRoleDisabled
	}
	return startLocalStream(ctx, cmd)
}

// ExecInteractive 对控制节点开本机 PTY 会话(容器内交互终端);其余透传。
// 非类 Unix 平台返回 ErrLocalInteractiveUnsupported(前端按人读错误展示)。
func (s *localHostService) ExecInteractive(ctx context.Context, serverID string, cmd []string) (Session, error) {
	if serverID != LocalHostID {
		return s.Service.ExecInteractive(ctx, serverID, cmd)
	}
	if !s.rolesEnabled() {
		return nil, ErrLocalRoleDisabled
	}
	return startLocalInteractive(ctx, cmd)
}

// Upload 对控制节点把 content 写到本机 remotePath(流式);其余透传。
// 与 SSH 版语义的差异:本机实现自动创建父目录(0o755,见下方 MkdirAll)—— 部署到本机
// 的常见目标是新目录,不建目录会直接失败;文件权限 0644。
func (s *localHostService) Upload(ctx context.Context, serverID string, content io.Reader, remotePath string) error {
	if serverID != LocalHostID {
		return s.Service.Upload(ctx, serverID, content, remotePath)
	}
	if !s.rolesEnabled() {
		return ErrLocalRoleDisabled
	}
	if remotePath == "" {
		return errors.New("target: empty local path")
	}
	if err := os.MkdirAll(filepath.Dir(remotePath), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(remotePath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, content); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Download 对控制节点把本机 remotePath 流式读回写入 w(与 SSH 版语义对齐,接口对称;
// 部署域不用,集群共享构建回拉为延展项);其余透传。文件不存在 → 透传 os 错误(人读)。
func (s *localHostService) Download(ctx context.Context, serverID, remotePath string, w io.Writer) (int64, error) {
	if serverID != LocalHostID {
		return s.Service.Download(ctx, serverID, remotePath, w)
	}
	if !s.rolesEnabled() {
		return 0, ErrLocalRoleDisabled
	}
	if w == nil {
		return 0, errors.New("target: nil writer")
	}
	if strings.TrimSpace(remotePath) == "" {
		return 0, errors.New("target: empty local path")
	}
	f, err := os.Open(remotePath)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	return io.Copy(w, f)
}

// DockerLogin 对控制节点在本机执行 `docker login [<registry>]`(口令经 stdin,绝不进
// argv/日志)。凭据经 RegistryCredentialProvider 可选接口取明文(装饰器拿不到 vault;
// 具体实现缺失 → 明确错误,不静默跳过);其余透传。
func (s *localHostService) DockerLogin(ctx context.Context, serverID, registryURL, credentialID string) error {
	if serverID != LocalHostID {
		return s.Service.DockerLogin(ctx, serverID, registryURL, credentialID)
	}
	if !s.rolesEnabled() {
		return ErrLocalRoleDisabled
	}
	if strings.TrimSpace(credentialID) == "" {
		return errors.New("target: docker login 缺少凭据")
	}
	provider, ok := s.Service.(RegistryCredentialProvider)
	if !ok {
		return errors.New("target: 本机 docker login 不可用:凭据提供者未装配")
	}
	user, pass, err := provider.OpenRegistryCredential(ctx, credentialID)
	if err != nil {
		return err
	}
	args := []string{"docker", "login"}
	if registryURL != "" {
		args = append(args, registryURL)
	}
	if user != "" {
		args = append(args, "-u", user)
	}
	args = append(args, "--password-stdin")
	res, runErr := runLocalCommand(ctx, args, strings.NewReader(pass+"\n"))
	// 明文用完即清(尽力而为;字符串不可清零,丢弃引用即可)。
	pass, user = "", ""
	_, _ = pass, user

	if runErr != nil {
		return runErr
	}
	if res != nil && res.ExitCode != 0 {
		return fmt.Errorf("target: docker login 失败(退出码 %d):%s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// runLocalCommand 在本机跑 cmd(array,不经 shell)。语义与 SSH 版对齐:
//   - 命令跑完(含非零退出)→ (*ExecResult, nil),由调用方看 ExitCode;
//   - 命令**无法启动**(不存在/不可执行)→ 伪退出码 127/126 + stderr,仍返回 nil error;
//   - ctx 超时/取消 → 返回 ctx.Err()(上层映射「采集超时」)。
func runLocalCommand(ctx context.Context, cmd []string, stdin io.Reader) (*ExecResult, error) {
	if len(cmd) == 0 {
		return nil, ErrEmptyCommand
	}
	c := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
	var out, errb bytes.Buffer
	c.Stdout = &out
	c.Stderr = &errb
	if stdin != nil {
		c.Stdin = stdin
	}
	err := c.Run()
	res := &ExecResult{Stdout: out.String(), Stderr: errb.String()}
	if err != nil {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
			return res, nil
		}
		// 启动失败:命令不存在 → 127;其余(权限/非可执行)→ 126。stderr 写人读提示,
		// 便于上层容器运行时探测把「本机无 docker」表现为「未检测到容器运行时」。
		res.ExitCode = localExitNotExecutable
		if errors.Is(err, exec.ErrNotFound) {
			res.ExitCode = localExitNotFound
		}
		res.Stderr = "无法启动本机命令:" + cmd[0]
		return res, nil
	}
	return res, nil
}

// localStream 是本机流式 stdout(供实时日志)。Close 会杀掉子进程并回收,幂等(可并发)。
type localStream struct {
	cmd  *exec.Cmd
	rc   io.ReadCloser
	done chan struct{}
	once sync.Once
}

// startLocalStream 启动本机命令并返回其 stdout 流。
func startLocalStream(ctx context.Context, cmd []string) (io.ReadCloser, error) {
	if len(cmd) == 0 {
		return nil, ErrEmptyCommand
	}
	c := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
	rc, err := c.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := c.Start(); err != nil {
		return nil, err
	}
	s := &localStream{cmd: c, rc: rc, done: make(chan struct{})}
	// ctx 取消(或调用方 Close)即回收子进程,防泄漏。
	go func() {
		select {
		case <-ctx.Done():
			_ = s.Close()
		case <-s.done:
		}
	}()
	return s, nil
}

func (s *localStream) Read(p []byte) (int, error) { return s.rc.Read(p) }

// Close 幂等:杀掉子进程、等待回收、关闭读端。
func (s *localStream) Close() error {
	s.once.Do(func() {
		close(s.done)
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		_ = s.cmd.Wait()
		_ = s.rc.Close()
	})
	return nil
}
