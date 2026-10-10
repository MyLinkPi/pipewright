package deploy

// strategy.go 实现「统一滚动部署」:预检(故障机优先排序)→ 分批(首批/每批台数可配)→
// 任一批失败立即停止铺开 → 每机按产物类型 + 网关托管情况执行,失败机**独立**回滚。
//
// 单机执行语义(deployOne)不变:
//   - image + 网关托管   → maxSurge 换实例(起新→预热健康→upstream 原子换→排空→停旧,见
//     instance_rolling.go;失败删新容器即可,旧实例未停,天然无需回滚);
//   - image 非网关托管   → pull → rm 旧 → run 新 → 健康门控 → 失败回滚上一镜像(image_release.go);
//   - dist/jar/archive   → 发布目录 + current 原子软链 → 重启命令 → 健康门控 → 失败回滚上一发布
//     (release.go;回滚后会尽力重跑重启命令);
//   - 命令型             → 直接执行 restartCommand。
//
// 网关联动(两种并存):网关托管机的文件/命令部署走「摘(upstream 摘除 + reload)→ 部署 →
// 健康通过 → 挂回」;部署失败保持摘除(故障机不回流量)。
//
// 安全不变量沿用:命令 array 化(不拼 shell)、单机 panic recover、有界并发(maxParallelDeploys)、
// message 无明文密钥、错误不上抛(映射 status + 人读)。

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/target"
)

// StrategyRolling 滚动发布(唯一保留策略):分批推进,各机独立成败、独立回滚。
const StrategyRolling = "rolling"

// NormalizeStrategy 归一策略串:canary / blue_green / interactive / instance_rolling / recreate 等
// 历史值**全部**归一为 rolling(存量配置兼容;策略收敛后滚动是唯一编排)。
func NormalizeStrategy(string) string {
	return StrategyRolling
}

// deployRolling 统一滚动编排(结果按 servers 输入顺序对齐,稳定可断言):
//
//  1. 预检排序(故障机优先):配置了健康检查 → 先对全部目标机逐台预检(复用 runHealthCheck),
//     预检不过的排到队首先修,其余保持稳定顺序;预检原因记入该机结果 message。端口型探测在
//     预检阶段仅当端口可静态推导(显式 healthPort / regPort)时执行 —— 自动分配端口起容器前不可知。
//  2. 分批:首批 = 排序后前 firstBatchSize 台(默认 1),之后每批 batchSize 台(默认全部剩余);
//     任一批有失败 → 立即停止,未轮到的机器记 pending(未部署,仍运行旧版本)。
//  3. 每机独立执行 + 独立回滚(deployOne;批次内并行 deployFanout)。
func (s *service) deployRolling(ctx context.Context, servers []*target.Server, a run.Artifact, cfg map[string]string, hsp *healthSpec) []TargetResult {
	total := len(servers)
	if total == 0 {
		return nil
	}

	// 1) 预检排序(故障机优先)。precheckReasons 记录预检未通过机器的人读原因。
	order := make([]int, total)
	for i := range order {
		order[i] = i
	}
	precheckReasons := map[int]string{}
	precheckHC := hsp.resolve(cfgNonNeg(cfg, "regPort"))
	if precheckHC.enabled() {
		failing := s.precheckFailing(ctx, servers, precheckHC)
		if len(failing) > 0 {
			ordered := make([]int, 0, total)
			for _, i := range order {
				if _, bad := failing[i]; bad {
					ordered = append(ordered, i)
				}
			}
			for _, i := range order {
				if _, bad := failing[i]; !bad {
					ordered = append(ordered, i)
				}
			}
			order = ordered
			for i, reason := range failing {
				precheckReasons[i] = reason
			}
		}
	}

	// 2) 分批推进。批次序列:首批 firstN 台,其后每批 batchM 台(batchM<=0 = 全部剩余一批推完)。
	firstN := firstBatchSize(cfg, total)
	batchM := rollingBatchSize(cfg)
	if batchM <= 0 {
		batchM = total
	}
	results := make([]TargetResult, total)
	stoppedAt := -1 // 首个出现失败的批次下标;-1 = 全部成功。
	for start := 0; start < total; {
		end := start + firstN
		if start > 0 {
			end = start + batchM
		}
		if end > total {
			end = total
		}
		batchIdx := order[start:end]
		batch := make([]*target.Server, 0, len(batchIdx))
		for _, i := range batchIdx {
			batch = append(batch, servers[i])
		}
		batchRes := s.deployFanout(ctx, batch, a, cfg, hsp)
		for j, r := range batchRes {
			i := batchIdx[j]
			if reason, bad := precheckReasons[i]; bad && r.Status == run.TargetFailed {
				r.Message = "预检健康未通过(" + reason + "),已优先安排本机滚动;部署结果:" + r.Message
			} else if reason, bad := precheckReasons[i]; bad {
				r.Message = "预检健康未通过(" + reason + "),已优先安排本机滚动;" + r.Message
			}
			results[i] = r
		}
		if !allSuccess(batchRes) {
			stoppedAt = end
			break
		}
		start = end
	}
	if stoppedAt >= 0 {
		// 任一批失败 → 停止铺开:未轮到的机器记 pending(未部署,仍运行旧版本)。
		// pending 属「可重试」状态(RetryFailed 会一并推进),修复失败机后即可继续铺开。
		now := time.Now().UTC()
		for _, i := range order[stoppedAt:] {
			results[i] = TargetResult{
				ServerID:   servers[i].ID,
				ServerName: servers[i].Name,
				Status:     run.TargetPending,
				Message:    "滚动部署在前一批失败后停止,本机未部署(仍运行旧版本);修复失败机后用「重试」继续铺开本机",
				StartedAt:  now,
			}
		}
	}
	return results
}

// precheckFailing 滚动前预检:对全部目标机用同一健康检查配置逐台探测(有界并发),
// 返回「预检未通过」的机器下标 → 人读原因。探测失败(连接不上)同样算预检未通过。
func (s *service) precheckFailing(ctx context.Context, servers []*target.Server, hc *HealthCheck) map[int]string {
	reasons := make(map[int]string, len(servers))
	var mu sync.Mutex
	s.forEachServer(servers, func(idx int, srv *target.Server) {
		perr := s.runHealthCheck(scopeCmdLog(ctx, srv.Name), srv.ID, hc)
		if perr == nil {
			return
		}
		mu.Lock()
		reasons[idx] = truncate(perr.Error())
		mu.Unlock()
	}, func(idx int, srv *target.Server) {
		mu.Lock()
		reasons[idx] = "预检执行异常中断"
		mu.Unlock()
	})
	return reasons
}

// firstBatchSize 解析首批机器数(cfg["firstBatchSize"];默认 1)。夹紧 [1, total]。
func firstBatchSize(cfg map[string]string, total int) int {
	n := 1
	if raw := strings.TrimSpace(cfg["firstBatchSize"]); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			n = v
		}
	}
	if n > total {
		n = total
	}
	if n < 1 {
		n = 1
	}
	return n
}

// rollingBatchSize 解析后续每批机器数(cfg["batchSize"];空 / 非法 → 0 = 全部剩余一批推完)。
func rollingBatchSize(cfg map[string]string) int {
	raw := strings.TrimSpace(cfg["batchSize"])
	if raw == "" {
		return 0
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		return 0
	}
	return v
}

// allSuccess 报告一批结果是否全为 success(批次门控判定)。
func allSuccess(results []TargetResult) bool {
	for i := range results {
		if results[i].Status != run.TargetSuccess {
			return false
		}
	}
	return len(results) > 0
}

// forEachServer 以有界并发(maxParallelDeploys)对每台 server 跑 work;每 goroutine recover 兜底,
// panic → 调用 onPanic(由其写入该机失败结果)。work / onPanic 各自写入自己的索引槽,不共享可变状态外的竞争。
func (s *service) forEachServer(servers []*target.Server, work func(idx int, srv *target.Server), onPanic func(idx int, srv *target.Server)) {
	sem := make(chan struct{}, maxParallelDeploys)
	var wg sync.WaitGroup
	for i := range servers {
		wg.Add(1)
		go func(idx int, srv *target.Server) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			defer func() {
				if rec := recover(); rec != nil {
					onPanic(idx, srv)
				}
			}()
			work(idx, srv)
		}(i, servers[i])
	}
	wg.Wait()
}
