package deploy

// touchrecorder_test.go 是部署测试的可控 SSH stub(原 strategy_test.go 的公共辅助,策略重写后
// 保留供 stage_image_test / release_test 等复用):按 (serverID, cmd) 注入失败,记录每机被执行的命令,
// 并模拟 readlink current(上一发布)/ docker inspect(上一镜像),使回滚路径可离线断言。

import (
	"context"
	"strings"
	"sync"

	"github.com/huangchengsir/pipewright/internal/target"
)

// recordingTarget 包装 stubTarget 语义:execFn 据 (serverID, cmd) 决定结果,并记录触达的 serverID。
type touchRecorder struct {
	mu           sync.Mutex
	touched      map[string]bool // 被 Exec 过的 serverID
	sawMv        bool            // 是否出现过 cutover 切换(mv -T → current)
	calls2       [][]string      // 全部命令(image 断言 pull/run 用)
	failOn       func(serverID string, cmd []string) bool
	prevLink     string // 非空 → readlink current 返回该路径(模拟已有上一发布)
	inspectImage string // 非空 → docker inspect 返回该镜像(模拟已有上一镜像)
}

func (r *touchRecorder) exec(serverID string, cmd []string) (*target.ExecResult, error) {
	r.mu.Lock()
	if r.touched == nil {
		r.touched = map[string]bool{}
	}
	r.touched[serverID] = true
	r.calls2 = append(r.calls2, cmd)
	if cmd[0] == "mv" {
		r.sawMv = true
	}
	r.mu.Unlock()
	// 模拟 readlink current → 上一发布(供机群回滚有 prev 可回)。
	if cmd[0] == "readlink" {
		if r.prevLink != "" {
			return &target.ExecResult{ExitCode: 0, Stdout: r.prevLink}, nil
		}
		return &target.ExecResult{ExitCode: 0}, nil
	}
	// 模拟 docker inspect → 上一镜像(供 image 机群回滚有 prevImage 可回)。
	if len(cmd) >= 2 && cmd[0] == "docker" && cmd[1] == "inspect" {
		if r.inspectImage != "" {
			return &target.ExecResult{ExitCode: 0, Stdout: r.inspectImage}, nil
		}
		return &target.ExecResult{ExitCode: 0}, nil
	}
	if r.failOn != nil && r.failOn(serverID, cmd) {
		return &target.ExecResult{ExitCode: 1, Stderr: "injected failure"}, nil
	}
	return &target.ExecResult{ExitCode: 0}, nil
}

// ctxCaptureTarget 包装 stubTarget:记录每条命令执行时收到的 ctx,供断言「健康门控 / 回滚 /
// docker pull 是否用独立预算的 ctx,而非共用切换阶段的 60s execCtx」(超时会话预算回归)。
type ctxCaptureTarget struct {
	*stubTarget
	mu   sync.Mutex
	ctxs map[string][]context.Context // key = 命令空格连接;值 = 每次执行收到的 ctx
}

func (c *ctxCaptureTarget) Exec(ctx context.Context, serverID string, cmd []string) (*target.ExecResult, error) {
	key := strings.Join(cmd, " ")
	c.mu.Lock()
	if c.ctxs == nil {
		c.ctxs = map[string][]context.Context{}
	}
	c.ctxs[key] = append(c.ctxs[key], ctx)
	c.mu.Unlock()
	return c.stubTarget.Exec(ctx, serverID, cmd)
}

// ctxAt 取 key 命令第 i 次执行时收到的 ctx(无则 nil)。
func (c *ctxCaptureTarget) ctxAt(key string, i int) context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	if i < len(c.ctxs[key]) {
		return c.ctxs[key][i]
	}
	return nil
}
