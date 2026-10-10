// cmdlog.go:把部署链路在目标机真实执行的命令 + 其 stdout/stderr 实时回流到运行步骤日志,
// 让 deploy_ssh 步骤像 Jenkins 控制台一样看得到「具体执行了什么」(此前仅一行结果摘要)。
//
// 脱敏分两道:① 命令回显时对紧跟 -e/--env/--build-arg 的 K=V、--password 的值就地打码(第一道);
// ② 文本最终经 sink 侧 per-run Masker 兜底脱敏(与构建日志同一机制,绝无明文 secret 落库/出网)。
package deploy

import (
	"context"
	"fmt"
	"strings"

	"github.com/huangchengsir/pipewright/internal/target"
)

const (
	cmdStreamStdout = "stdout"
	cmdStreamStderr = "stderr"
)

// CmdLogFunc 接收一行待写入运行日志的部署命令/输出(stream = stdout | stderr)。
// machine 是该行的来源机器显示名("":运行级/控制机,或由 scopeCmdLog 注入单机归属)。
type CmdLogFunc func(stream, machine, text string)

type cmdLogKey struct{}

// WithCmdLog 把命令日志回调挂到 ctx:部署链路每条远程命令 + 其输出经它实时回流。
// fn 为 nil → 原样返回(不需要命令级日志的调用方,如 /runs/{id}/deploy 端点,无副作用)。
func WithCmdLog(ctx context.Context, fn CmdLogFunc) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, cmdLogKey{}, fn)
}

func cmdLogFrom(ctx context.Context) CmdLogFunc {
	if fn, ok := ctx.Value(cmdLogKey{}).(CmdLogFunc); ok && fn != nil {
		return fn
	}
	return func(string, string, string) {}
}

// scopeCmdLog 返回单机作用域子 ctx:深层经 cmdLogFrom 发射的行自动带上 machine 归属,
// 逐机执行点(deployFanout 并行体 / runCommandOnly 循环体 / 逐机探测循环)包一次即可,
// 深处(s.exec、端口映射、登录告警等)零改动获得归属。
func scopeCmdLog(ctx context.Context, machine string) context.Context {
	base := cmdLogFrom(ctx)
	return WithCmdLog(ctx, func(stream, _, text string) { base(stream, machine, text) })
}

// exec 包裹 targets.Exec:执行前回显命令、执行后回流 stdout/stderr 到 ctx 的命令日志(若挂了)。
// 返回值与 targets.Exec 完全一致,不改变任何控制流(无 ctx 日志时 = 纯透传)。
// 机器归属由上层单机作用域(scopeCmdLog)注入,此处只透传 machine=""(交由作用域填)。
func (s *service) exec(ctx context.Context, serverID string, cmd []string) (*target.ExecResult, error) {
	return execObserved(ctx, s.targets.Exec, serverID, cmd)
}

// execObserved 执行一条 array 命令并把「命令 + 输出」回流到 ctx 的命令日志(若挂了):
// 命令以 `$ ` 前缀回显、执行错误以 `  ✗ ` 前缀、stdout/stderr 原样回流、非零退出码显式标注。
// s.exec(部署链路)与 observingTarget.Exec(第三方编排组件)共用同一口径,日志格式全局一致。
func execObserved(ctx context.Context, run func(context.Context, string, []string) (*target.ExecResult, error), serverID string, cmd []string) (*target.ExecResult, error) {
	lg := cmdLogFrom(ctx)
	lg(cmdStreamStdout, "", "$ "+displayCmd(cmd))
	out, err := run(ctx, serverID, cmd)
	if err != nil {
		lg(cmdStreamStderr, "", "  ✗ "+humanExecError(err))
		return out, err
	}
	if out != nil {
		if t := strings.Trim(out.Stdout, "\r\n"); strings.TrimSpace(t) != "" {
			lg(cmdStreamStdout, "", t)
		}
		if t := strings.Trim(out.Stderr, "\r\n"); strings.TrimSpace(t) != "" {
			lg(cmdStreamStderr, "", t)
		}
		if out.ExitCode != 0 {
			lg(cmdStreamStderr, "", fmt.Sprintf("  ✗ 退出码 %d", out.ExitCode))
		}
	}
	return out, err
}

// observingTarget 装饰 target.Service:每条 Exec 的命令与输出回流到 ctx 上的命令日志(若有);
// 其余方法(Get/List/Upload/DockerLogin/…)**匿名嵌入**原样透传。
type observingTarget struct{ target.Service }

// ObservingTarget 返回「命令可见」的 target.Service 包装:每条经 Exec 的目标机命令 + stdout/stderr
// 在 ctx 挂有命令日志(WithCmdLog)时回流;无日志 ctx = 纯透传(行为与裸 target.Service 逐字节一致)。
//
// 用途:servicereg 等底层编排组件各自直接持有 target.Service(deploy 不 import servicereg),
// 其 SSH 命令(网关 nginx 容器编排等)默认不出现于运行日志;main 装配时用它包一层,这些命令即经
// 部署链路注入的日志回调进入步骤日志(「流水线日志可见所有执行的命令」)。
//
// 机器归属:沿用调用方 ctx 上的单机作用域(部署路径为触发本动作的目标机);后台/管理页调用
// 无作用域 → 运行级,不误导来源。
func ObservingTarget(inner target.Service) target.Service {
	if inner == nil {
		return nil
	}
	return observingTarget{Service: inner}
}

func (o observingTarget) Exec(ctx context.Context, serverID string, cmd []string) (*target.ExecResult, error) {
	return execObserved(ctx, o.Service.Exec, serverID, cmd)
}

// displayCmd 把 array 命令拼为可读单行;对敏感参数值就地打码(第一道脱敏)。
//   - sh -c <script> [args...]:直接展示脚本本体(docker build/run 等,可读性最佳;
//     脚本内 secret 由 sink 侧 Masker 兜底)。
//   - 其余:逐 token 拼接,紧跟 -e/--env/--build-arg 的 K=V → K=***,--password 的值 → ***。
func displayCmd(cmd []string) string {
	if len(cmd) >= 3 && cmd[0] == "sh" && cmd[1] == "-c" {
		return strings.TrimSpace(cmd[2])
	}
	out := make([]string, len(cmd))
	for i, tok := range cmd {
		out[i] = tok
		if i == 0 {
			continue
		}
		switch cmd[i-1] {
		case "-e", "--env", "--build-arg", "--arg":
			out[i] = maskAssign(tok)
		case "--password", "--password-stdin":
			out[i] = "***"
		}
	}
	return strings.Join(out, " ")
}

// maskAssign 把 "K=V" → "K=***"(只暴露 key);无 '=' 原样返回。
func maskAssign(kv string) string {
	if i := strings.IndexByte(kv, '='); i >= 0 {
		return kv[:i+1] + "***"
	}
	return kv
}
