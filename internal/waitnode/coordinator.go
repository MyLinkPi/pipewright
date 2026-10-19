// Package waitnode 是「等待节点」(type=wait)的进程内协调器:节点执行期 sleep 固定秒数
// (解决部署时序),期间可经 HTTP 端点手动跳过。
//
// 模型与审批门(internal/approval)同构:执行器在开始等待时经 Coordinator.Wait 注册等待者,
// skip 端点经 Coordinator.Resolve 投递「跳过」决定;定时到 / 跳过 / 运行取消三路 select 竞争。
// 与审批门的差异:等待不改变 run 状态(其他阶段/节点照常并行),仅影响该节点自身;跳过后
// 节点 step 落 skipped 但按成功放行下游。
//
// 边界(与审批门一致,诚实声明):等待者只存内存,进程重启即丢——重启清理(FailOrphanedRuns)
// 会把残留 running 运行落失败。等待有上限(maxWaitSeconds),不会永久占用 worker。
package waitnode

import (
	"sort"
	"sync"
	"time"
)

// Skip 是一次手动跳过决定。
type Skip struct {
	Actor string // 操作人(展示/审计;绝无敏感数据)
}

// WaitInfo 描述一个正在等待中的节点(注册时快照;剩余时长按 Deadline 即时计算)。
type WaitInfo struct {
	Ordinal      int    // 全局 step 序号(-1 = 未知,前端按名回退匹配)
	JobID        string // job id(跳过端点的定位键)
	JobName      string // 展示名
	StageName    string // 所属阶段名
	TotalSeconds int    // 配置的总等待秒数
	Deadline     time.Time
}

// RemainingSeconds 返回截至此刻的剩余整秒(下限 0)。
func (w WaitInfo) RemainingSeconds() int {
	d := int(time.Until(w.Deadline).Seconds())
	if d < 0 {
		return 0
	}
	return d
}

// Coordinator 是进程内等待节点协调器(零持久状态;无 DB 依赖、无迁移)。
type Coordinator struct {
	mu      sync.Mutex
	waiters map[string]waiterEntry
}

type waiterEntry struct {
	ch   chan Skip
	info WaitInfo
}

// New 构造协调器。
func New() *Coordinator {
	return &Coordinator{waiters: make(map[string]waiterEntry)}
}

// Key 由 runID + jobID 组成等待唯一键(job id 在 spec 内全局唯一,保存期校验保证)。
func Key(runID, jobID string) string { return runID + "|" + jobID }

// runPrefix 返回某 run 全部等待键的公共前缀。
func runPrefix(runID string) string { return runID + "|" }

// Wait 为 key 注册一个等待,返回接收跳过决定的只读 channel(缓冲 1,Resolve 不阻塞)。
// 同 key 重复 Wait 覆盖旧等待者(旧 channel 永不收到决定;调用方应在 defer 里 Cancel 清理)。
func (c *Coordinator) Wait(key string, info WaitInfo) <-chan Skip {
	ch := make(chan Skip, 1)
	c.mu.Lock()
	c.waiters[key] = waiterEntry{ch: ch, info: info}
	c.mu.Unlock()
	return ch
}

// Resolve 把跳过决定投递给等待 key 的节点。无等待者(未开始等待 / 已到时 / 已跳过) → false。
// 投递后移除等待者(同一等待只决一次)。
func (c *Coordinator) Resolve(key string, s Skip) bool {
	c.mu.Lock()
	e, ok := c.waiters[key]
	if ok {
		delete(c.waiters, key)
	}
	c.mu.Unlock()
	if !ok {
		return false
	}
	e.ch <- s // 缓冲 1,不阻塞
	return true
}

// Cancel 移除 key 的等待者(等待自然结束 / 取消时清理)。幂等。
func (c *Coordinator) Cancel(key string) {
	c.mu.Lock()
	delete(c.waiters, key)
	c.mu.Unlock()
}

// PendingByRun 返回某运行当前等待中的节点列表(按 ordinal 升序;供运行详情端点展示)。
func (c *Coordinator) PendingByRun(runID string) []WaitInfo {
	c.mu.Lock()
	prefix := runPrefix(runID)
	infos := make([]WaitInfo, 0, 4)
	for k, e := range c.waiters {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			infos = append(infos, e.info)
		}
	}
	c.mu.Unlock()
	sort.Slice(infos, func(i, j int) bool { return infos[i].Ordinal < infos[j].Ordinal })
	return infos
}
