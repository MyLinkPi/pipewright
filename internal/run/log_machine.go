package run

// log_machine.go 是日志行的「来源机器归属」通道(步骤 × 机器分组展示):
// 产生逐机输出的执行层(deploy 逐机部署/探测、远程构建节点)经 WithLogMachine 把机器显示名
// 挂到 ctx,dbStepSink.Log 落库时读出并写入 run_logs.machine —— StepSink.Log 等跨层接口
// 签名保持不变(与 deploy.WithCmdLog 同一套 ctx 钩子风格,build/dagrun 全链路零改动透传)。
//
// 语义:machine = 机器显示名快照(部署/取机时刻);"" = 控制机本机执行或运行级日志(缺省)。
// 机器名非敏感(展示名),不参与脱敏(脱敏只作用于 text)。

import "context"

// logMachineKey 是 ctx 里机器归属的键(私有结构体防碰撞)。
type logMachineKey struct{}

// WithLogMachine 返回携带机器归属的子 ctx:此后经该 ctx 上报的 Log 行都会落到该机器名下。
func WithLogMachine(ctx context.Context, machine string) context.Context {
	if machine == "" {
		return ctx
	}
	return context.WithValue(ctx, logMachineKey{}, machine)
}

// LogMachineFrom 读出 ctx 上的机器归属(无 → 空串 = 控制机/运行级)。
func LogMachineFrom(ctx context.Context) string {
	if v, ok := ctx.Value(logMachineKey{}).(string); ok {
		return v
	}
	return ""
}
