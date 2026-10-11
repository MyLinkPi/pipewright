package dagrun

import (
	"context"
	"strings"
	"sync"
)

// node_status.go 维护「本次运行内各节点(job)的实时状态」注册表,供执行器在**运行中途**
// 读取其它节点状态(典型消费方 = 通知节点的 {{status}} 占位:内联 notify 节点执行时运行
// 尚未结束,run.Status 恒为 running,只有节点级状态才回答「上游那个节点到底成没成」)。
//
// 数据来源与生命周期:
//   - Runner.Run 在开跑前建表并**预置全部节点为 pending**(按节点恢复的继承/人工跳过节点
//     直接预置 success/skipped,与 Plan 落库的初始态一致)。
//   - stageReporter 在节点上报 JobRunning/JobDone(含 finishRemaining 兜底收尾)时同步更新。
//   - 表挂在**本次运行的 ctx** 上(WithNodeStatusRegistry),运行结束即随 ctx 释放 ——
//     执行器是跨运行复用的闭包,绝不能把状态存进执行器自身(会串运行 / 泄漏)。
//
// 并发:阶段/job 可并行执行,读写全部经互斥锁;状态值是 run 包的步骤状态枚举
// (pending/running/success/failed/skipped)。
type NodeStatusRegistry struct {
	mu    sync.Mutex
	byID  map[string]*nodeStatus
	order []*nodeStatus // 声明序,保证同名多命中时的聚合结果稳定可复现
}

// nodeStatus 是表内一行:节点 id(唯一)+ 展示名(配置引用用)+ 当前状态。
type nodeStatus struct {
	id     string
	name   string
	status string
}

// NewNodeStatusRegistry 建空表(Runner.Run 每运行一个)。
func NewNodeStatusRegistry() *NodeStatusRegistry {
	return &NodeStatusRegistry{byID: map[string]*nodeStatus{}}
}

// Add 预置一个节点(jobID + 展示名 + 初始状态)。重复 Add 同 id 覆盖(防御,正常不会发生)。
func (r *NodeStatusRegistry) Add(jobID, name, status string) {
	if r == nil {
		return
	}
	id := strings.TrimSpace(jobID)
	if id == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.byID[id]; ok {
		cur.name, cur.status = strings.TrimSpace(name), status
		return
	}
	e := &nodeStatus{id: id, name: strings.TrimSpace(name), status: status}
	r.byID[id] = e
	r.order = append(r.order, e)
}

// Update 更新某节点的当前状态(running / 终态;仅 id,未知 id 静默忽略)。
func (r *NodeStatusRegistry) Update(jobID, status string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.byID[strings.TrimSpace(jobID)]; ok {
		e.status = status
	}
}

// StatusByName 按节点**展示名**取当前状态(通知节点配置 statusFrom 即按名引用,与部署节点
// artifactJob 同一惯例;名字对 YAML/画布可读,且在矩阵展开后依然稳定——cell 只改 id 不改名)。
//
// 同名多命中(如矩阵展开出的多个 cell 共用一个 job 名)按「最需关注」优先聚合:
// failed > running > pending > skipped > success。这样任何一个 cell 失败都会如实报 failed。
// 未命中 → ("", false)。
func (r *NodeStatusRegistry) StatusByName(name string) (string, bool) {
	if r == nil {
		return "", false
	}
	want := strings.TrimSpace(name)
	if want == "" {
		return "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	matched := false
	best := ""
	for _, e := range r.order {
		if e.name != want {
			continue
		}
		matched = true
		if best == "" || statusAttentionRank(e.status) < statusAttentionRank(best) {
			best = e.status
		}
	}
	return best, matched
}

// statusAttentionRank 是聚合优先级(越小越「需关注」)。未知状态排在最后(不遮蔽已知状态)。
func statusAttentionRank(status string) int {
	switch status {
	case "failed":
		return 0
	case "running":
		return 1
	case "pending":
		return 2
	case "skipped":
		return 3
	case "success":
		return 4
	default:
		return 5
	}
}

// nodeStatusRegistryKey 是 ctx 里注册表的键(私有结构体防碰撞,同 run.WithLogMachine 的做法)。
type nodeStatusRegistryKey struct{}

// WithNodeStatusRegistry 返回携带节点状态注册表的子 ctx。Runner.Run 在调度阶段前注入,
// 执行器经 NodeStatusFrom 读取。
func WithNodeStatusRegistry(ctx context.Context, reg *NodeStatusRegistry) context.Context {
	return context.WithValue(ctx, nodeStatusRegistryKey{}, reg)
}

// NodeStatusFrom 读出 ctx 上的节点状态注册表;未注入(如单测直接调执行器)→ nil。
func NodeStatusFrom(ctx context.Context) *NodeStatusRegistry {
	if v, ok := ctx.Value(nodeStatusRegistryKey{}).(*NodeStatusRegistry); ok {
		return v
	}
	return nil
}
