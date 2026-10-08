package target

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// dialTimeout 是 TCP 拨号 + SSH 握手的兜底超时(ctx 无 deadline 时生效)。
const dialTimeout = 10 * time.Second

// sshDialer 是基于 golang.org/x/crypto/ssh 的默认 SSHDialer 实现。
//
// AC-SEC-02:cmd 以 []string(程序 + 参数)传入,经 quoteArgs 对各参数 POSIX shell 转义
// 后再交 session.Run。调用方**不**拼接原始 shell 字符串,杜绝命令注入(参数中的
// `; rm -rf /`、`$(...)`、反引号等被当作字面量,绝不被远端 shell 解释执行)。
//
// HostKeyCallback:本期 dev 用 InsecureIgnoreHostKey 以便对 localhost 真测。
// ⚠️ DEFERRED(生产硬性):生产必须固定 known_hosts(ssh.FixedHostKey / knownhosts.New),
// 否则无法防 MITM。本机自测场景无 known_hosts,故暂放宽;切勿带此设置上生产。
type sshDialer struct{}

func (sshDialer) Run(ctx context.Context, addr string, cfg SSHConfig, cmd []string) (*ExecResult, error) {
	client, closeChain, err := dialClient(ctx, addr, cfg)
	if err != nil {
		return nil, err
	}
	defer closeChain()

	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("%w", ErrUnreachable)
	}
	defer func() { _ = session.Close() }()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr

	// AC-SEC-02:array → 安全转义的单行命令(各参数 %q POSIX 转义),不让调用方拼 shell。
	runErr := runWithContext(ctx, session, quoteArgs(cmd))

	res := &ExecResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}

	if runErr != nil {
		var exitErr *ssh.ExitError
		if errors.As(runErr, &exitErr) {
			// 远端命令以非零退出:不是连接错误,exitCode 据实回传。
			res.ExitCode = exitErr.ExitStatus()
			return res, nil
		}
		if errors.Is(runErr, context.DeadlineExceeded) || errors.Is(runErr, context.Canceled) {
			return nil, runErr
		}
		return nil, fmt.Errorf("%w", ErrUnreachable)
	}
	res.ExitCode = 0
	return res, nil
}

// RunWithStdin 同 Run,但把 stdin 接到远端命令标准输入(供 `cat > file` 流式上传产物字节)。
// 与 Run 同样的连接/认证/超时/退出码语义;stdin 读尽即由 session 关闭远端 stdin。
func (sshDialer) RunWithStdin(ctx context.Context, addr string, cfg SSHConfig, cmd []string, stdin io.Reader) (*ExecResult, error) {
	client, closeChain, err := dialClient(ctx, addr, cfg)
	if err != nil {
		return nil, err
	}
	defer closeChain()

	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("%w", ErrUnreachable)
	}
	defer func() { _ = session.Close() }()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr
	session.Stdin = stdin // 产物字节经 stdin 流式喂给远端 `cat > file`(无 argv 长度限)。

	runErr := runWithContext(ctx, session, quoteArgs(cmd))
	res := &ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if runErr != nil {
		var exitErr *ssh.ExitError
		if errors.As(runErr, &exitErr) {
			res.ExitCode = exitErr.ExitStatus()
			return res, nil
		}
		if errors.Is(runErr, context.DeadlineExceeded) || errors.Is(runErr, context.Canceled) {
			return nil, runErr
		}
		return nil, fmt.Errorf("%w", ErrUnreachable)
	}
	res.ExitCode = 0
	return res, nil
}

// RunStream 建连后在 session 上启动 cmd 并返回 stdout 流。底层 client/session 的生命周期
// 绑定到返回的 ReadCloser:Close()(或 ctx 取消)即关 session + client,释放远端进程与 TCP。
//
// AC-SEC-02:cmd 经 quoteArgs 各参数 POSIX 转义后 Start,杜绝注入(同 Run)。
func (sshDialer) RunStream(ctx context.Context, addr string, cfg SSHConfig, cmd []string) (io.ReadCloser, error) {
	client, closeChain, err := dialClient(ctx, addr, cfg)
	if err != nil {
		return nil, err
	}

	session, err := client.NewSession()
	if err != nil {
		closeChain()
		return nil, fmt.Errorf("%w", ErrUnreachable)
	}

	stdout, err := session.StdoutPipe()
	if err != nil {
		_ = session.Close()
		closeChain()
		return nil, fmt.Errorf("%w", ErrUnreachable)
	}

	if err := session.Start(quoteArgs(cmd)); err != nil {
		_ = session.Close()
		closeChain()
		return nil, fmt.Errorf("%w", ErrUnreachable)
	}

	rc := &streamReadCloser{
		r:       stdout,
		session: session,
		chain:   closeChain,
	}

	// ctx 取消时主动收尾(客户端断开 / 超时):杀远端进程并关 session/client,防泄漏挂死。
	stop := make(chan struct{})
	rc.stop = stop
	go func() {
		select {
		case <-ctx.Done():
			rc.Close()
		case <-stop:
		}
	}()

	return rc, nil
}

// RunInteractive 建连后请求 PTY、接好 stdin/stdout/stderr,启动 cmd 并返回双向 Session
// (Story 6.4;FR-18)。底层 client/session 的生命周期绑定到返回的 Session:Close()(或
// ctx 取消)即关 session + client,释放远端 PTY/进程与 TCP(不泄漏)。
//
// AC-SEC-02:cmd 经 quoteArgs 各参数 POSIX 转义后作为单行命令 Start(同 Run/RunStream),
// 杜绝注入(`docker exec -it <id> <shell>` 各参数被当字面量,绝不被远端 shell 二次解释)。
func (sshDialer) RunInteractive(ctx context.Context, addr string, cfg SSHConfig, cmd []string) (Session, error) {
	client, closeChain, err := dialClient(ctx, addr, cfg)
	if err != nil {
		return nil, err
	}

	session, err := client.NewSession()
	if err != nil {
		closeChain()
		return nil, fmt.Errorf("%w", ErrUnreachable)
	}

	stdin, err := session.StdinPipe()
	if err != nil {
		_ = session.Close()
		closeChain()
		return nil, fmt.Errorf("%w", ErrUnreachable)
	}
	// stdout + stderr 合流到同一个管道(交互终端里二者本就交织呈现)。
	pr, pw := io.Pipe()
	session.Stdout = pw
	session.Stderr = pw

	// 请求 PTY。xterm + 合理初值;后续 WindowChange 据前端 fit 调整。
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := session.RequestPty("xterm-256color", 24, 80, modes); err != nil {
		_ = session.Close()
		closeChain()
		return nil, fmt.Errorf("%w", ErrUnreachable)
	}

	if err := session.Start(quoteArgs(cmd)); err != nil {
		_ = session.Close()
		closeChain()
		return nil, fmt.Errorf("%w", ErrUnreachable)
	}

	is := &interactiveSession{
		stdin:   stdin,
		out:     pr,
		outW:    pw,
		session: session,
		chain:   closeChain,
	}

	// 远端进程退出 → 关 pw 让读侧得到 EOF(否则 Read 永久阻塞)。
	go func() {
		_ = session.Wait()
		_ = pw.Close()
	}()

	// ctx 取消(客户端断开 / 超时)→ 主动收尾,防泄漏挂死。
	stop := make(chan struct{})
	is.stop = stop
	go func() {
		select {
		case <-ctx.Done():
			_ = is.Close()
		case <-stop:
		}
	}()

	return is, nil
}

// interactiveSession 实现 target.Session:把 SSH PTY 会话包成双向 ReadWriteCloser + Resize。
// Close 幂等(防 ctx 协程与调用方并发关 + 多次关)。
type interactiveSession struct {
	stdin   io.WriteCloser
	out     io.Reader // io.Pipe 读端(stdout+stderr 合流)
	outW    io.Closer // io.Pipe 写端(Close 时一并关,解阻塞 Wait 协程外的 Read)
	session *ssh.Session
	chain   func() // 整链收尾(逆序关最外层 client + 全部中间跳)
	stop    chan struct{}
	once    sync.Once
}

func (s *interactiveSession) Read(p []byte) (int, error)  { return s.out.Read(p) }
func (s *interactiveSession) Write(p []byte) (int, error) { return s.stdin.Write(p) }

// Resize 通知远端 PTY 调整窗口(列 cols × 行 rows)。非法尺寸归一为 1;失败忽略(非致命)。
func (s *interactiveSession) Resize(cols, rows int) error {
	if cols <= 0 {
		cols = 1
	}
	if rows <= 0 {
		rows = 1
	}
	return s.session.WindowChange(rows, cols)
}

func (s *interactiveSession) Close() error {
	s.once.Do(func() {
		if s.stop != nil {
			close(s.stop)
		}
		// 先发 SIGKILL 杀远端 shell/容器 exec(否则远端进程驻留);忽略错误。
		_ = s.session.Signal(ssh.SIGKILL)
		_ = s.stdin.Close()
		_ = s.session.Close()
		s.chain() // 关整链(含中间跳,防泄漏)
		_ = s.outW.Close() // 解阻塞任何在 Read 上等待的读者
	})
	return nil
}

// streamReadCloser 把 SSH session 的 stdout 包成 ReadCloser,Close 时杀远端进程并关 session
// 与整条连接链(含中间跳)。Close 幂等(防多次关 + ctx 协程与调用方并发关)。
type streamReadCloser struct {
	r       io.Reader
	session *ssh.Session
	chain   func() // 整链收尾(逆序关最外层 client + 全部中间跳)
	stop    chan struct{}
	once    sync.Once
}

func (s *streamReadCloser) Read(p []byte) (int, error) { return s.r.Read(p) }

func (s *streamReadCloser) Close() error {
	s.once.Do(func() {
		if s.stop != nil {
			close(s.stop)
		}
		// 先发 SIGKILL 杀远端 tail -f / journalctl -f(否则远端进程驻留);忽略错误。
		_ = s.session.Signal(ssh.SIGKILL)
		_ = s.session.Close()
		s.chain() // 关整链(含中间跳,防泄漏)
	})
	return nil
}

// runWithContext 在 session 上跑 cmd,并让 ctx 取消/超时能中断阻塞的 Run。
func runWithContext(ctx context.Context, session *ssh.Session, cmd string) error {
	done := make(chan error, 1)
	go func() { done <- session.Run(cmd) }()
	select {
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGKILL)
		_ = session.Close()
		return ctx.Err()
	case err := <-done:
		return err
	}
}

// hopError 标注错误发生在跳板链第几跳(label 仅含跳位序号与主机:端口,不含原始
// OS/握手文本,维持「不泄敏感细节」纪律)。Unwrap 保持 errors.Is 到 ErrUnreachable /
// ErrAuth 等 sentinel;humanError 据 label 在人读文案里透出跳位,便于定位哪一跳失败。
type hopError struct {
	label string
	err   error
}

func (e *hopError) Error() string { return e.label + ": " + e.err.Error() }
func (e *hopError) Unwrap() error { return e.err }

// dialClient 经可选跳板链(cfg.Jumps,按连接顺序)建立到 addr 的 SSH 连接,返回最外层
// client 与整链收尾闭包。链路:本机 → Jumps[0] → … → Jumps[n-1] → addr;第 0 跳直接
// TCP 拨号,其后每跳经上一跳 client 的转发通道 Dial("tcp", …) 取得底层 net.Conn 再做
// SSH 握手(等价 `ssh -J hop1 -J hop2 target`)。Jumps 为空时行为与单跳直连完全一致。
//
// 返回的 closer 幂等无害且按建链逆序关闭**全部** client(含最外层)——调用方以此替代
// 单点 client.Close,防中间跳连接泄漏。任一跳失败即回收已建立的跳;非目标跳的错误包
// hopError(带跳位),目标跳保持既有错误形态(不加前缀,兼容旧语义与既有测试)。
// HostKeyCallback 沿用 InsecureIgnoreHostKey(同单跳,见 sshDialer 类型注释 DEFERRED)。
func dialClient(ctx context.Context, addr string, cfg SSHConfig) (*ssh.Client, func(), error) {
	timeout := resolveTimeout(ctx)

	// 逐跳装配:每跳各自独立的认证材料;最后一跳即目标。
	type hopSpec struct {
		addr string
		cfg  SSHConfig
	}
	specs := make([]hopSpec, 0, len(cfg.Jumps)+1)
	for _, j := range cfg.Jumps {
		specs = append(specs, hopSpec{
			addr: j.Addr,
			cfg:  SSHConfig{User: j.User, PrivateKey: j.PrivateKey, Password: j.Password},
		})
	}
	specs = append(specs, hopSpec{
		addr: addr,
		cfg:  SSHConfig{User: cfg.User, PrivateKey: cfg.PrivateKey, Password: cfg.Password},
	})

	var chain []*ssh.Client
	// rawConns 是各跳的原始 socket:握手未完成/通道未开时链上还没有 client 可关,
	// 唯一的拆除手段就是 socket 级关闭。stops 撤销尚未触发的取消回调(正常路径不留悬挂)。
	var rawConns []net.Conn
	var stops []func() bool
	closeChain := func() {
		for _, stop := range stops {
			stop()
		}
		for i := len(chain) - 1; i >= 0; i-- {
			_ = chain[i].Close()
		}
		for _, c := range rawConns {
			_ = c.Close()
		}
	}

	// dialConn 取得某跳的底层 net.Conn:首跳走 TCP(尊重 ctx 取消/超时);其后经上一跳
	// client 转发(client.Dial 不接 ctx,包 goroutine + select 保证可被 ctx 打断)。
	dialConn := func(via *ssh.Client, target string) (net.Conn, error) {
		if via == nil {
			d := net.Dialer{Timeout: timeout}
			return d.DialContext(ctx, "tcp", target)
		}
		type dialRes struct {
			conn net.Conn
			err  error
		}
		ch := make(chan dialRes, 1)
		go func() {
			c, err := via.Dial("tcp", target)
			ch <- dialRes{c, err}
		}()
		select {
		case r := <-ch:
			return r.conn, r.err
		case <-ctx.Done():
			// 迟到的成功结果由 reaper 关闭;channel 缓冲 1,goroutine 不会泄漏。
			go func() {
				if r := <-ch; r.conn != nil {
					_ = r.conn.Close()
				}
			}()
			return nil, ctx.Err()
		}
	}

	var via *ssh.Client
	for i, sp := range specs {
		isTarget := i == len(specs)-1
		auth, err := authMethods(sp.cfg)
		if err != nil {
			closeChain()
			if isTarget {
				return nil, nil, err
			}
			return nil, nil, &hopError{label: hopLabel(i, sp.addr), err: err}
		}
		clientCfg := &ssh.ClientConfig{
			User:            sp.cfg.User,
			Auth:            auth,
			HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // dev-only;生产固定 known_hosts(deferred)
			Timeout:         timeout,
		}
		conn, err := dialConn(via, sp.addr)
		if err != nil {
			closeChain()
			if isTarget {
				return nil, nil, classifyDialErr(err)
			}
			return nil, nil, &hopError{label: hopLabel(i, sp.addr), err: classifyDialErr(err)}
		}
		rawConns = append(rawConns, conn)
		// ctx 取消 → 立即关本跳原始 socket:阻塞中的 NewClientConn/NewSession 不认 ctx,
		// 只有 socket 级拆除能让传输 goroutine 即刻收敛(对齐上游 dialSSH 的取消语义)。
		stops = append(stops, context.AfterFunc(ctx, func() { _ = conn.Close() }))
		sshConn, chans, reqs, err := ssh.NewClientConn(conn, sp.addr, clientCfg)
		if err != nil {
			_ = conn.Close()
			closeChain()
			if isTarget {
				return nil, nil, classifyHandshakeErr(err)
			}
			return nil, nil, &hopError{label: hopLabel(i, sp.addr), err: classifyHandshakeErr(err)}
		}
		cl := ssh.NewClient(sshConn, chans, reqs)
		chain = append(chain, cl)
		via = cl
	}
	return chain[len(chain)-1], closeChain, nil
}

// hopLabel 构造跳位标签(仅序号 + 主机:端口,无凭据/原始错误文本)。
func hopLabel(i int, addr string) string {
	return fmt.Sprintf("第%d跳(%s)", i+1, addr)
}

// authMethods 据 cfg 装配 SSH 认证法(私钥优先,否则口令)。明文仅进程内,不外泄。
func authMethods(cfg SSHConfig) ([]ssh.AuthMethod, error) {
	if cfg.PrivateKey != "" {
		signer, err := ssh.ParsePrivateKey([]byte(cfg.PrivateKey))
		if err != nil {
			// 解析失败:不回显私钥内容,只给干净领域错误。
			return nil, ErrInvalidCredential
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
	}
	if cfg.Password != "" {
		return []ssh.AuthMethod{ssh.Password(cfg.Password)}, nil
	}
	return nil, ErrInvalidCredential
}

// resolveTimeout 取 ctx 剩余时间作为拨号超时;无 deadline 时用 dialTimeout 兜底。
func resolveTimeout(ctx context.Context) time.Duration {
	if dl, ok := ctx.Deadline(); ok {
		if d := time.Until(dl); d > 0 {
			return d
		}
		return time.Millisecond // 已超时:让拨号立即失败
	}
	return dialTimeout
}

// classifyDialErr 把 TCP 拨号错误映射为干净领域错误(绝不含内部地址/栈)。
func classifyDialErr(err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return ErrUnreachable
	}
	return ErrUnreachable
}

// classifyHandshakeErr 把 SSH 握手错误映射:认证类 → ErrAuth,其余 → ErrUnreachable。
// 仅看错误**类别**,绝不透传可能含敏感细节的原始文本。
func classifyHandshakeErr(err error) error {
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "unable to authenticate") ||
		strings.Contains(msg, "auth") ||
		strings.Contains(msg, "permission denied") ||
		strings.Contains(msg, "no supported methods") {
		return ErrAuth
	}
	return ErrUnreachable
}

// quoteArgs 把命令 array 转为安全的单行 shell 命令:程序名 + 各参数经 POSIX 单引号转义。
// AC-SEC-02 防注入核心:参数里的特殊字符(空格、`;`、`$()`、反引号、引号)全被当字面量。
func quoteArgs(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = shellQuote(a)
	}
	return strings.Join(parts, " ")
}

// shellQuote 对单个参数做 POSIX 单引号转义。空串 → ”。内部单引号用 '\” 序列拼接。
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	// 若全为安全字符,免引号(更可读;不影响安全)。
	safe := true
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '-' || r == '_' || r == '.' || r == '/' || r == ':' || r == '=' || r == '@') {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
