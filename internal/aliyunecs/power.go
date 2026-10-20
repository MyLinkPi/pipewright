package aliyunecs

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/huangchengsir/pipewright/internal/runner"
)

// 开关机的轮询节奏与护栏。
const (
	powerPollInterval = 5 * time.Second
	// powerMaxWaitSeconds 是等待到达目标状态的最长时间(防节点挂死整个 stage)。
	powerMaxWaitSeconds = 3600
)

// 目标状态:开机等 Running,关机等 Stopped。
const (
	statusRunning  = "Running"
	statusStopped  = "Stopped"
	statusStarting = "Starting"
	statusStopping = "Stopping"
)

// PowerInput 是流水线 ecs_power 节点的执行入参(整批统一动作)。
type PowerInput struct {
	AccountID string
	// Selector 决定机器选择方式:instances(直选,用 InstanceIDs)| tags(按标签运行时解析)。
	Selector string
	// TagSource 仅 Selector=tags 时生效:aliyun(ECS 标签)| platform(平台服务器标签,
	// 匹配后经实例映射反查;非本账户导入的匹配机器跳过并告警)。
	TagSource string
	// Tags 是标签选择条件(多对之间 AND;value 空 = 只要 key 存在)。
	Tags map[string]string
	// InstanceIDs 是直选实例(Snapshot:配置时选定,运行时校验存在)。
	InstanceIDs []string
	// Action 是 start | stop;stop 固定节省停机模式(StoppedMode=StopCharging)。
	Action string
	// ForceStop 仅 stop 有意义:强制断电式停机(可能丢未落盘数据)。
	ForceStop bool
	// WaitSeconds 是等待到达目标状态的最长时间(0 = 发起即成功,不等待)。
	WaitSeconds int
	// Log 可选流式日志回调(节点日志);nil 安全。绝不应写入凭据。
	Log func(msg string)
}

// PowerItemResult 是单台实例的开关机结果(独立容错)。
type PowerItemResult struct {
	InstanceID string
	ServerID   string // 仅平台标签来源时有值(匹配到的 servers 行)
	Skipped    bool   // 已在目标状态,未发起操作(幂等跳过)
	OK         bool
	Err        string
}

// PowerResult 是整批开关机汇总。
type PowerResult struct {
	Items  []PowerItemResult
	Total  int
	OK     int
	Failed int
}

func (in PowerInput) logf(format string, args ...any) {
	if in.Log != nil {
		in.Log(fmt.Sprintf(format, args...))
	}
}

// matchesAliyunTags 判断实例的阿里云标签是否命中全部条件(AND;value 空 = key 存在即可)。
func matchesAliyunTags(inst Instance, tags map[string]string) bool {
	for k, want := range tags {
		got, ok := inst.Tags[k]
		if !ok {
			return false
		}
		if want != "" && got != want {
			return false
		}
	}
	return true
}

// lookupInstanceByServer 反查「服务器行 → 该账户下的实例 ID」(经导入映射);无映射返回空串。
func (s *service) lookupInstanceByServer(ctx context.Context, accountID, serverID string) string {
	var instanceID string
	err := s.db.QueryRowContext(ctx,
		`SELECT instance_id FROM aliyun_ecs_instance_links WHERE account_id = ? AND server_id = ?`,
		accountID, serverID).Scan(&instanceID)
	if err != nil {
		return ""
	}
	return instanceID
}

// resolveTargets 把三种选择方式统一解析为目标实例清单(带可选 serverID 归属)。
// tags 两种来源均为运行时解析;0 命中 → ErrNoTagMatch(整批失败,防标签拼错静默空跑)。
func (s *service) resolveTargets(ctx context.Context, cli ecsAPI, acc *Account, in PowerInput) (map[string]string, error) {
	switch in.Selector {
	case "", SelectorInstances:
		targets := make(map[string]string, len(in.InstanceIDs))
		for _, id := range in.InstanceIDs {
			targets[id] = ""
		}
		return targets, nil

	case SelectorTags:
		if len(in.Tags) == 0 {
			return nil, fmt.Errorf("%w:标签条件为空", ErrNoTagMatch)
		}
		if in.TagSource == "" || in.TagSource == TagSourceAliyun {
			// 阿里云 ECS 标签:拉全量实例后客户端过滤(统一 AND/key-only 语义,
			// 不走 Tag.N 服务端过滤,规避 RPC 数组参数格式分歧)。
			instances, err := cli.ListInstances(ctx)
			if err != nil {
				return nil, wrapPower(err, "拉取实例列表失败")
			}
			targets := make(map[string]string)
			for _, inst := range instances {
				if matchesAliyunTags(inst, in.Tags) {
					targets[inst.InstanceID] = ""
				}
			}
			if len(targets) == 0 {
				return nil, fmt.Errorf("%w:阿里云标签 %v 未匹配到任何实例", ErrNoTagMatch, tagSummary(in.Tags))
			}
			return targets, nil
		}
		if in.TagSource == TagSourcePlatform {
			// 平台服务器标签:与构建机池同一匹配语义(runner.MatchSelector,AND),
			// 命中后经导入映射反查实例 ID;非阿里云导入的匹配机器跳过并告警。
			terms := tagTerms(in.Tags)
			servers, err := s.servers.List(ctx)
			if err != nil {
				return nil, fmt.Errorf("aliyunecs: list servers: %w", err)
			}
			targets := make(map[string]string)
			skipped := 0
			for _, srv := range servers {
				if !runner.MatchSelector(srv.Labels, terms) {
					continue
				}
				instID := s.lookupInstanceByServer(ctx, acc.ID, srv.ID)
				if instID == "" {
					skipped++
					in.logf("服务器 %s(%s) 命中标签但非本账户导入的 ECS,跳过", srv.Name, srv.Host)
					continue
				}
				targets[instID] = srv.ID
			}
			if len(targets) == 0 {
				if skipped > 0 {
					return nil, fmt.Errorf("%w:服务器标签 %v 匹配到 %d 台,但均非本账户导入的 ECS", ErrNoTagMatch, terms, skipped)
				}
				return nil, fmt.Errorf("%w:服务器标签 %v 未匹配到任何服务器", ErrNoTagMatch, terms)
			}
			if skipped > 0 {
				in.logf("服务器标签匹配:%d 台可操作,%d 台非导入机器已跳过", len(targets), skipped)
			}
			return targets, nil
		}
		return nil, fmt.Errorf("%w:非法标签来源 %q", ErrPowerFailed, in.TagSource)

	default:
		return nil, fmt.Errorf("%w:非法选择方式 %q", ErrPowerFailed, in.Selector)
	}
}

// tagSummary 把标签条件折成日志友好形式(key=value 或 key=*)。
func tagSummary(tags map[string]string) string {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if v := tags[k]; v != "" {
			parts = append(parts, k+"="+v)
		} else {
			parts = append(parts, k+"=*")
		}
	}
	return strings.Join(parts, ",")
}

// tagTerms 把平台标签条件折为 runner 选择器项(k=v 或裸 key)。
func tagTerms(tags map[string]string) []string {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	terms := make([]string, 0, len(keys))
	for _, k := range keys {
		if v := tags[k]; v != "" {
			terms = append(terms, k+"="+v)
		} else {
			terms = append(terms, k)
		}
	}
	return terms
}

// wrapPower 把底层错误折进开关机错误族,保留人话尾巴(绝不回显 ak/sk)。
func wrapPower(err error, msg string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrPowerFailed) {
		return err
	}
	return fmt.Errorf("%w:%s(%v)", ErrPowerFailed, msg, err)
}

// targetStatus 返回动作的目标状态与转换中状态。
func targetStatus(action string) (want, transitioning string, ok bool) {
	switch action {
	case PowerActionStart:
		return statusRunning, statusStarting, true
	case PowerActionStop:
		return statusStopped, statusStopping, true
	}
	return "", "", false
}

// Power 执行开关机(解析选择 → 查当前态跳过已达标 → 逐台发起 → 可选等待)。
// 逐台独立容错:一台失败不阻断其它台;返回逐台结果(任一台失败由调用方裁决)。
func (s *service) Power(ctx context.Context, in PowerInput) (*PowerResult, error) {
	if _, _, ok := targetStatus(in.Action); !ok {
		return nil, fmt.Errorf("%w:非法动作 %q(仅 start|stop)", ErrPowerFailed, in.Action)
	}
	if in.WaitSeconds < 0 || in.WaitSeconds > powerMaxWaitSeconds {
		return nil, fmt.Errorf("%w:等待时间须为 0..%d 秒", ErrPowerFailed, powerMaxWaitSeconds)
	}
	cli, acc, err := s.clientFor(ctx, in.AccountID)
	if err != nil {
		return nil, err
	}
	targets, err := s.resolveTargets(ctx, cli, acc, in)
	if err != nil {
		return nil, err
	}
	if len(targets) > MaxPowerBatch {
		return nil, fmt.Errorf("%w:单批最多 %d 台(解析到 %d 台)", ErrPowerFailed, MaxPowerBatch, len(targets))
	}
	// 稳定顺序(逐台结果可复现)。
	ids := make([]string, 0, len(targets))
	for id := range targets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	want, transitioning, _ := targetStatus(in.Action)
	actionLabel := "开机"
	if in.Action == PowerActionStop {
		actionLabel = "关机(节省停机模式)"
	}
	in.logf("ECS %s:共 %d 台 [%s]", actionLabel, len(ids), strings.Join(ids, ", "))

	// 查当前态:已在目标状态 → 幂等跳过;转换中 → 不再发起、直接进等待。
	statuses, err := cli.InstanceStatuses(ctx, ids)
	if err != nil {
		return nil, wrapPower(err, "查询实例状态失败")
	}
	res := &PowerResult{Total: len(ids), Items: make([]PowerItemResult, 0, len(ids))}
	pending := make([]string, 0, len(ids)) // 已发起或转换中,等待到达目标态
	for _, id := range ids {
		item := PowerItemResult{InstanceID: id, ServerID: targets[id]}
		switch statuses[id] {
		case want:
			item.OK = true
			item.Skipped = true
			in.logf("%s 已是 %s,跳过", id, want)
		case transitioning:
			item.OK = true // 发起前置为成功;是否真达标由等待裁决
			pending = append(pending, id)
			in.logf("%s 正在 %s,直接等待", id, transitioning)
		case "":
			item.Err = "实例不存在(可能已释放或不属于该账户)"
		default:
			if err := s.powerOne(ctx, cli, in, id); err != nil {
				item.Err = err.Error()
			} else {
				item.OK = true
				pending = append(pending, id)
				in.logf("%s 已发起%s", id, actionLabel)
			}
		}
		if item.OK {
			res.OK++
		} else {
			res.Failed++
		}
		res.Items = append(res.Items, item)
	}

	// 可选等待:轮询直到 pending 全部到达目标态或超时(超时台改判失败)。
	if in.WaitSeconds > 0 && len(pending) > 0 {
		deadline := time.Now().Add(time.Duration(in.WaitSeconds) * time.Second)
		for len(pending) > 0 {
			wait := powerPollInterval
			if remain := time.Until(deadline); remain <= 0 {
				s.finishPowerTimeout(res, statuses, pending, want)
				break
			} else if remain < wait {
				wait = remain
			}
			select {
			case <-ctx.Done():
				// 取消(流水线中止):未确认的台按取消时刻状态收尾,不判成功。
				s.finishPowerTimeout(res, statuses, pending, want)
				return res, nil
			case <-time.After(wait):
			}
			cur, err := cli.InstanceStatuses(ctx, pending)
			if err != nil {
				// 轮询失败不立即放弃(可能是瞬时抖动);下次轮询再试,超时统一裁决。
				in.logf("状态轮询失败(将在下轮重试):%v", err)
				continue
			}
			for k, v := range cur {
				statuses[k] = v
			}
			still := make([]string, 0, len(pending))
			for _, id := range pending {
				if statuses[id] == want {
					in.logf("%s 已到达 %s", id, want)
				} else {
					still = append(still, id)
				}
			}
			pending = still
			if len(pending) == 0 {
				break
			}
			in.logf("等待 %d 台到达 %s(当前 %s…)", len(pending), want, summarizeStatuses(statuses, pending))
		}
	}
	return res, nil
}

// powerOne 对单台发起动作(错误折进开关机错误族,人话无凭据)。
func (s *service) powerOne(ctx context.Context, cli ecsAPI, in PowerInput, instanceID string) error {
	var err error
	if in.Action == PowerActionStart {
		err = cli.StartInstance(ctx, instanceID)
	} else {
		err = cli.StopInstance(ctx, instanceID, in.ForceStop)
	}
	if err != nil {
		return wrapPower(err, instanceID+" 操作被阿里云拒绝")
	}
	return nil
}

// finishPowerTimeout 把仍未确认的 pending 台按超时收尾(改判失败,带最后已知状态)。
func (s *service) finishPowerTimeout(res *PowerResult, statuses map[string]string, pending []string, want string) {
	if len(pending) == 0 {
		return
	}
	stillSet := make(map[string]bool, len(pending))
	for _, id := range pending {
		stillSet[id] = true
	}
	for i := range res.Items {
		it := &res.Items[i]
		if !stillSet[it.InstanceID] || !it.OK {
			continue
		}
		last := statuses[it.InstanceID]
		if last == want {
			continue
		}
		it.OK = false
		res.OK--
		res.Failed++
		if last == "" {
			it.Err = "等待超时且状态未知"
		} else {
			it.Err = "等待超时(最后状态 " + last + ")"
		}
	}
}

// summarizeStatuses 汇总 pending 台的状态分布(等待日志一行)。
func summarizeStatuses(statuses map[string]string, pending []string) string {
	counts := make(map[string]int)
	for _, id := range pending {
		st := statuses[id]
		if st == "" {
			st = "未知"
		}
		counts[st]++
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s×%d", k, counts[k]))
	}
	return strings.Join(parts, ", ")
}
