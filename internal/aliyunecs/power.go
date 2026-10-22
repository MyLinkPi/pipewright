package aliyunecs

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/huangchengsir/pipewright/internal/runner"
	"github.com/huangchengsir/pipewright/internal/target"
)

// 开关机的轮询节奏与护栏。
const (
	powerPollInterval = 5 * time.Second
	// powerMaxWaitSeconds 是等待到达目标状态的最长时间(防节点挂死整个 stage)。
	powerMaxWaitSeconds = 3600
	// probeDialTimeout 是单次 TCP 探活的连接超时(每轮探测一台一次,不放大总等待)。
	probeDialTimeout = 3 * time.Second
	// defaultProbePort 是 waitReachable 的兜底探测端口(SSH)。
	defaultProbePort = 22
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
	// WaitReachable 仅 Action=start 且 WaitSeconds>0 时生效:实例到达 Running 后继续
	// 等待网络可达(TCP 探测 host:port),全部可达或超时才收尾;探测地址优先导入
	// servers 行的连接地址(与 deploy_ssh 同源),兜底阿里云公网/私网 IP。
	WaitReachable bool
	// ReachablePort 是 TCP 探活端口(1..65535;0 = 22)。实例已关联 servers 行时
	// 优先用行上端口。
	ReachablePort int
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

// serverIDsByInstances 批量反查「实例 → servers 行」映射(经导入映射;仅返回命中的条目)。
// 查询失败不致命:调用方兜底走阿里云 IP 解析。
func (s *service) serverIDsByInstances(ctx context.Context, accountID string, instanceIDs []string) map[string]string {
	out := make(map[string]string, len(instanceIDs))
	if len(instanceIDs) == 0 {
		return out
	}
	ph := make([]string, len(instanceIDs))
	args := make([]any, 0, len(instanceIDs)+1)
	args = append(args, accountID)
	for i, id := range instanceIDs {
		ph[i] = "?"
		args = append(args, id)
	}
	q := `SELECT instance_id, server_id FROM aliyun_ecs_instance_links ` +
		`WHERE account_id = ? AND instance_id IN (` + strings.Join(ph, ",") + `)`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return out
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var instID, srvID string
		if err := rows.Scan(&instID, &srvID); err == nil && srvID != "" {
			out[instID] = srvID
		}
	}
	return out
}

// probePlan 是一台待探活实例的探测地址(解析一次缓存复用;noAddr 为解析终态)。
type probePlan struct {
	host   string
	port   int
	noAddr bool // 无可探测地址(记原因判失败,不再等)
	reason string
}

// resolveProbePlans 为待探活实例解析 TCP 探测地址(批量,一次)。
// 优先级:导入 servers 行 Host:Port(导入时选定的连接地址,与后续 deploy_ssh 同源;
// 带跳板的行控制机不可直连,忽略)→ 阿里云公网 IP → 私网 IP;端口兜底 fallbackPort
// (<=0 归一 22),servers 行自带端口优先。返回仅含本轮能定论的台(含 noAddr);
// 依赖查询失败(servers.List / ListInstances)的台不出现,留待下轮再解析、超时统一裁决。
func (s *service) resolveProbePlans(ctx context.Context, cli ecsAPI, accountID string, ids []string, fallbackPort int) map[string]*probePlan {
	port := fallbackPort
	if port <= 0 {
		port = defaultProbePort
	}
	plans := make(map[string]*probePlan, len(ids))
	srvByInst := s.serverIDsByInstances(ctx, accountID, ids)
	if len(srvByInst) > 0 {
		srvRows := make(map[string]*target.Server)
		if servers, err := s.servers.List(ctx); err == nil {
			for _, srv := range servers {
				srvRows[srv.ID] = srv
			}
			for instID, srvID := range srvByInst {
				srv := srvRows[srvID]
				if srv == nil || strings.TrimSpace(srv.Host) == "" || len(srv.Jumps) > 0 {
					continue
				}
				p := port
				if srv.Port > 0 {
					p = srv.Port
				}
				plans[instID] = &probePlan{host: strings.TrimSpace(srv.Host), port: p}
			}
		}
	}
	var missing []string
	for _, id := range ids {
		if plans[id] == nil {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		instances, err := cli.ListInstances(ctx)
		if err == nil {
			byID := make(map[string]Instance, len(instances))
			for _, inst := range instances {
				byID[inst.InstanceID] = inst
			}
			for _, id := range missing {
				inst, ok := byID[id]
				if !ok {
					plans[id] = &probePlan{noAddr: true, reason: "实例不在账户实例列表"}
					continue
				}
				host := firstNonEmpty(inst.PublicIPs...)
				if host == "" {
					host = firstNonEmpty(inst.PrivateIPs...)
				}
				if host == "" {
					plans[id] = &probePlan{noAddr: true, reason: "云上无公网/私网 IP"}
				} else {
					plans[id] = &probePlan{host: host, port: port}
				}
			}
		}
	}
	return plans
}

// firstNonEmpty 返回第一个去空白后非空的串(都没有 → 空串)。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

// probeTCPDial 是生产 TCP 探活实现:单次连接尝试,连接建立即视为可达(立即关闭)。
func probeTCPDial(ctx context.Context, host string, port int, timeout time.Duration) bool {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// failPowerItem 把已计入成功的逐台结果改判失败(条目必须在 Items 中且 OK=true)。
func failPowerItem(res *PowerResult, instanceID, reason string) {
	for i := range res.Items {
		it := &res.Items[i]
		if it.InstanceID != instanceID || !it.OK {
			continue
		}
		it.OK = false
		it.Err = reason
		res.OK--
		res.Failed++
		return
	}
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

// Power 执行开关机(解析选择 → 查当前态跳过已达标 → 逐台发起 → 可选等待,开机可选等
// 网络可达)。逐台独立容错:一台失败不阻断其它台;返回逐台结果(任一台失败由调用方裁决)。
func (s *service) Power(ctx context.Context, in PowerInput) (*PowerResult, error) {
	if _, _, ok := targetStatus(in.Action); !ok {
		return nil, fmt.Errorf("%w:非法动作 %q(仅 start|stop)", ErrPowerFailed, in.Action)
	}
	if in.WaitSeconds < 0 || in.WaitSeconds > powerMaxWaitSeconds {
		return nil, fmt.Errorf("%w:等待时间须为 0..%d 秒", ErrPowerFailed, powerMaxWaitSeconds)
	}
	if in.ReachablePort < 0 || in.ReachablePort > 65535 {
		return nil, fmt.Errorf("%w:探测端口须为 0..65535(0=默认 %d)", ErrPowerFailed, defaultProbePort)
	}
	// 可达等待只在开机时成立:等待预算与状态等待共用 waitSeconds。
	waitReach := in.WaitReachable
	if waitReach {
		if in.Action != PowerActionStart {
			return nil, fmt.Errorf("%w:waitReachable 仅开机(start)可用", ErrPowerFailed)
		}
		if in.WaitSeconds <= 0 {
			return nil, fmt.Errorf("%w:waitReachable 需 waitSeconds > 0 作为等待预算", ErrPowerFailed)
		}
	}
	probe := s.probeTCP
	if probe == nil {
		probe = probeTCPDial
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
	probing := make([]string, 0, len(ids)) // 已达目标态、待确认网络可达(仅 waitReach)
	for _, id := range ids {
		item := PowerItemResult{InstanceID: id, ServerID: targets[id]}
		switch statuses[id] {
		case want:
			if !waitReach {
				item.OK = true
				item.Skipped = true
				in.logf("%s 已是 %s,跳过", id, want)
				break
			}
			// 可达等待:已在目标状态不发起动作,但可达性仍由探测裁决(防「Running 而
			// 服务未起」被幂等跳过误判成功)。
			item.OK = true
			probing = append(probing, id)
			in.logf("%s 已是 %s,继续确认网络可达", id, want)
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

	// 可选等待:轮询直到 pending 全部到达目标态、probing 全部可达,或超时(超时台改判失败)。
	if in.WaitSeconds > 0 && (len(pending) > 0 || len(probing) > 0) {
		deadline := time.Now().Add(time.Duration(in.WaitSeconds) * time.Second)
		resolved := make(map[string]*probePlan) // 探测地址缓存(懒解析,一次一台)
		for len(pending) > 0 || len(probing) > 0 {
			wait := powerPollInterval
			if remain := time.Until(deadline); remain <= 0 {
				s.finishPowerTimeout(res, statuses, pending, want)
				finishProbeTimeout(res, probing, resolved, want)
				break
			} else if remain < wait {
				wait = remain
			}
			select {
			case <-ctx.Done():
				// 取消(流水线中止):未确认的台按取消时刻状态收尾,不判成功。
				s.finishPowerTimeout(res, statuses, pending, want)
				finishProbeTimeout(res, probing, resolved, want)
				return res, nil
			case <-time.After(wait):
			}
			if len(pending) > 0 {
				cur, err := cli.InstanceStatuses(ctx, pending)
				if err != nil {
					// 轮询失败不立即放弃(可能是瞬时抖动);下次轮询再试,超时统一裁决。
					in.logf("状态轮询失败(将在下轮重试):%v", err)
				} else {
					for k, v := range cur {
						statuses[k] = v
					}
					still := make([]string, 0, len(pending))
					for _, id := range pending {
						if statuses[id] == want {
							if waitReach {
								probing = append(probing, id)
								in.logf("%s 已到达 %s,确认网络可达", id, want)
							} else {
								in.logf("%s 已到达 %s", id, want)
							}
						} else {
							still = append(still, id)
						}
					}
					pending = still
				}
			}
			if len(probing) > 0 {
				var toResolve []string
				for _, id := range probing {
					if _, ok := resolved[id]; !ok {
						toResolve = append(toResolve, id)
					}
				}
				if len(toResolve) > 0 {
					for id, pl := range s.resolveProbePlans(ctx, cli, acc.ID, toResolve, in.ReachablePort) {
						resolved[id] = pl
					}
				}
				still := make([]string, 0, len(probing))
				for _, id := range probing {
					pl := resolved[id]
					if pl == nil {
						still = append(still, id) // 地址解析依赖暂不可用,下轮再试
						continue
					}
					if pl.noAddr {
						failPowerItem(res, id, "已是 "+want+",但无可探测地址("+pl.reason+")")
						continue
					}
					if probe(ctx, pl.host, pl.port, probeDialTimeout) {
						in.logf("%s 网络可达 %s:%d", id, pl.host, pl.port)
					} else {
						still = append(still, id)
					}
				}
				probing = still
			}
			if len(pending) == 0 && len(probing) == 0 {
				break
			}
			if len(pending) > 0 {
				in.logf("等待 %d 台到达 %s(当前 %s…)", len(pending), want, summarizeStatuses(statuses, pending))
			}
			if len(probing) > 0 {
				in.logf("等待 %d 台网络可达(每 %s 重试)…", len(probing), powerPollInterval)
			}
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

// finishProbeTimeout 把已达目标状态但未确认可达的 probing 台按超时收尾(改判失败,
// 带最后探测地址;地址未解析的用通用文案)。
func finishProbeTimeout(res *PowerResult, probing []string, plans map[string]*probePlan, want string) {
	for _, id := range probing {
		pl := plans[id]
		if pl != nil && !pl.noAddr {
			failPowerItem(res, id, fmt.Sprintf("等待超时(%s,但 %s:%d 不可达)", want, pl.host, pl.port))
		} else {
			failPowerItem(res, id, "等待超时("+want+",网络可达未确认)")
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

// ─── 服务器状态页电源入口(HTTP PowerServer / LinkedPowerStatus) ──────────────

// serverLink 是一条「servers 行 → 账户实例」映射(aliyun_ecs_instance_links 一行)。
type serverLink struct {
	AccountID  string
	InstanceID string
	ServerID   string
}

// linksForServer 反查服务器行关联的全部映射(账户+实例稳定排序)。
// 主键是 (account_id, instance_id),同一服务器行理论可挂多账户实例(逐个独立容错)。
func (s *service) linksForServer(ctx context.Context, serverID string) ([]serverLink, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT account_id, instance_id FROM aliyun_ecs_instance_links WHERE server_id = ? ORDER BY account_id, instance_id`,
		serverID)
	if err != nil {
		return nil, fmt.Errorf("aliyunecs: list links for server: %w", err)
	}
	defer rows.Close()
	var out []serverLink
	for rows.Next() {
		var lk serverLink
		if err := rows.Scan(&lk.AccountID, &lk.InstanceID); err != nil {
			return nil, fmt.Errorf("aliyunecs: scan link: %w", err)
		}
		lk.ServerID = serverID
		out = append(out, lk)
	}
	return out, rows.Err()
}

// PowerServer 对某登记服务器关联的 ECS 实例发起开机/关机(服务器状态页电源按钮消费)。
// 反查导入映射后逐账户走 Power(直选实例、WaitSeconds=0 发起即返回、幂等跳过已达标);
// 单账户失败折为该实例的逐台结果(不 500、不阻断其余账户)。结构错误(动作非法 / 无映射)
// 才返回 error。关机沿用 Power 的固定策略:节省停机模式,不强断。
func (s *service) PowerServer(ctx context.Context, serverID, action string) (*PowerResult, error) {
	if _, _, ok := targetStatus(action); !ok {
		return nil, fmt.Errorf("%w:非法动作 %q(仅 start|stop)", ErrPowerFailed, action)
	}
	links, err := s.linksForServer(ctx, serverID)
	if err != nil {
		return nil, err
	}
	if len(links) == 0 {
		return nil, fmt.Errorf("%w:服务器 %s 未关联任何导入的阿里云 ECS 实例", ErrServerNotLinked, serverID)
	}
	merged := &PowerResult{Total: len(links), Items: make([]PowerItemResult, 0, len(links))}
	for _, lk := range links {
		res, err := s.Power(ctx, PowerInput{
			AccountID:   lk.AccountID,
			Selector:    SelectorInstances,
			InstanceIDs: []string{lk.InstanceID},
			Action:      action,
			WaitSeconds: 0,
		})
		if err != nil {
			// 账户级失败(凭据/网络/账户不存在)折为该实例的逐台失败,人话无凭据。
			merged.Failed++
			merged.Items = append(merged.Items, PowerItemResult{
				InstanceID: lk.InstanceID,
				ServerID:   lk.ServerID,
				Err:        wrapPower(err, lk.InstanceID+" 开关机发起失败").Error(),
			})
			continue
		}
		merged.OK += res.OK
		merged.Failed += res.Failed
		for i := range res.Items {
			res.Items[i].ServerID = lk.ServerID // 直选路径领域层不填,此处回填归属
		}
		merged.Items = append(merged.Items, res.Items...)
	}
	return merged, nil
}

// LinkedPowerItem 是一台已导入 ECS 关联服务器的实时电源状态(查询结果,无动作)。
type LinkedPowerItem struct {
	ServerID   string
	AccountID  string
	InstanceID string
	Status     string // Running | Stopped | Starting | Stopping | ...(阿里云原值;空 = 未知)
	Err        string // 该条查询失败的人读错误(账户解封失败 / API 失败);成功为空
}

// LinkedPowerStatus 返回全部「已导入 ECS ↔ servers 行」映射及其实时电源状态
// (服务器状态页电源徽标数据源)。按账户分组批量 DescribeInstanceStatus,
// 单账户失败不拖垮其余(该账户名下条目带 Err、Status 为空)。
func (s *service) LinkedPowerStatus(ctx context.Context) ([]LinkedPowerItem, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT account_id, instance_id, server_id FROM aliyun_ecs_instance_links ORDER BY server_id, account_id, instance_id`)
	if err != nil {
		return nil, fmt.Errorf("aliyunecs: list links: %w", err)
	}
	var links []serverLink
	for rows.Next() {
		var lk serverLink
		if err := rows.Scan(&lk.AccountID, &lk.InstanceID, &lk.ServerID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("aliyunecs: scan link: %w", err)
		}
		links = append(links, lk)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("aliyunecs: list links: %w", err)
	}
	rows.Close()

	// 按账户分组,一组一次 API(同账户同 Region 的实例状态可批量查)。
	byAccount := make(map[string][]serverLink)
	for _, lk := range links {
		byAccount[lk.AccountID] = append(byAccount[lk.AccountID], lk)
	}
	statusByLink := make(map[serverLink]string, len(links))
	errByAccount := make(map[string]string, len(byAccount))
	accIDs := make([]string, 0, len(byAccount))
	for accID := range byAccount {
		accIDs = append(accIDs, accID)
	}
	sort.Strings(accIDs)
	for _, accID := range accIDs {
		group := byAccount[accID]
		ids := make([]string, 0, len(group))
		for _, lk := range group {
			ids = append(ids, lk.InstanceID)
		}
		cli, _, err := s.clientFor(ctx, accID)
		if err != nil {
			errByAccount[accID] = humanLinkErr(err)
			continue
		}
		statuses, err := cli.InstanceStatuses(ctx, ids)
		if err != nil {
			errByAccount[accID] = wrapPower(err, "查询实例状态失败").Error()
			continue
		}
		for _, lk := range group {
			statusByLink[lk] = statuses[lk.InstanceID] // 缺失(实例已释放)→ 空串 = 未知
		}
	}

	out := make([]LinkedPowerItem, 0, len(links))
	for _, lk := range links {
		item := LinkedPowerItem{
			ServerID:   lk.ServerID,
			AccountID:  lk.AccountID,
			InstanceID: lk.InstanceID,
			Status:     statusByLink[lk],
			Err:        errByAccount[lk.AccountID],
		}
		out = append(out, item)
	}
	return out, nil
}

// humanLinkErr 把映射状态查询的账户级错误折为人读(绝无凭据)。
func humanLinkErr(err error) string {
	switch {
	case errors.Is(err, ErrNotFound):
		return "账户不存在(可能已删除)"
	case errors.Is(err, ErrVaultUnconfigured):
		return "保险库未配置,无法解封 AccessKeySecret"
	default:
		return wrapPower(err, "账户不可用").Error()
	}
}
