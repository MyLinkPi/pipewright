package pipeline

import (
	"fmt"
	"strconv"
	"strings"
)

// ecs_power.go 校验阿里云 ECS 开关机节点(ecs_power)的配置形态。
//
// 该节点对一台或若干台 ECS 实例执行开机/关机(关机固定节省停机模式),机器选择有三种方式:
//   - selector=instances:config["instanceIds"] 逗号串(配置时快照);
//   - selector=tags + tagSource=aliyun:config["tags"] 按阿里云 ECS 标签运行时解析;
//   - selector=tags + tagSource=platform:同上,但按平台服务器标签(servers.labels,
//     与构建机池同一语义)匹配后经导入映射反查实例。
//
// 保存期只校验形态(枚举/非空/规模/标签项格式),不触库不触网:账户存在性与实例有效性由
// 运行时执行器裁决(账户可能后配,标签选择天然是运行时解析)。
const (
	ecsPowerType             = "ecs_power"
	ecsPowerActionKey        = "action"
	ecsPowerAccountIDKey     = "accountId"
	ecsPowerSelectorKey      = "selector"
	ecsPowerInstanceIDsKey   = "instanceIds"
	ecsPowerTagSourceKey     = "tagSource"
	ecsPowerTagsKey          = "tags"
	ecsPowerWaitSecondsKey   = "waitSeconds"
	ecsPowerWaitReachableKey = "waitReachable"
	ecsPowerReachablePortKey = "reachablePort"
	ecsPowerMaxInstances     = 100
	ecsPowerMaxTagTerms      = 32
	// ecsPowerMaxWaitSeconds 与运行时护栏(powerMaxWaitSeconds)对齐的等待上限。
	ecsPowerMaxWaitSeconds = 3600
	// ecsPowerMaxPort 是 reachablePort 上限。
	ecsPowerMaxPort = 65535
)

// validateEcsPower 校验全部 ecs_power 节点(错误归一为 ErrInvalidJob)。
func validateEcsPower(stages []Stage) error {
	for si := range stages {
		for ji := range stages[si].Jobs {
			n := stages[si].Jobs[ji]
			if strings.TrimSpace(n.Type) != ecsPowerType {
				continue
			}
			if err := validateEcsPowerJob(n); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateEcsPowerJob(n Job) error {
	cfgStr := func(key string) (string, bool) {
		raw, ok := n.Config[key]
		if !ok || raw == nil {
			return "", false
		}
		s, isStr := raw.(string)
		return s, isStr
	}
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: ecs_power job %q: "+format, append([]any{ErrInvalidJob, n.Name}, args...)...)
	}

	// action:开/关机二选一(缺省视为关机由前端表单保证;后端仍要求显式合法)。
	action, _ := cfgStr(ecsPowerActionKey)
	switch strings.TrimSpace(action) {
	case "start", "stop":
	default:
		return bad("action must be \"start\" or \"stop\"")
	}

	// accountId:必填(引用 设置→阿里云 ECS 的账户)。
	if accountID, ok := cfgStr(ecsPowerAccountIDKey); !ok || strings.TrimSpace(accountID) == "" {
		return bad("accountId is required (select an Aliyun account)")
	}

	// selector:instances(缺省)| tags。
	selector, _ := cfgStr(ecsPowerSelectorKey)
	selector = strings.TrimSpace(selector)
	if selector == "" {
		selector = "instances"
	}
	switch selector {
	case "instances":
		raw, ok := cfgStr(ecsPowerInstanceIDsKey)
		if !ok || strings.TrimSpace(raw) == "" {
			return bad("instanceIds is required when selector=instances")
		}
		ids := 0
		for _, p := range strings.Split(raw, ",") {
			if strings.TrimSpace(p) != "" {
				ids++
			}
		}
		if ids == 0 {
			return bad("instanceIds is required when selector=instances")
		}
		if ids > ecsPowerMaxInstances {
			return bad("instanceIds exceeds %d instances per batch", ecsPowerMaxInstances)
		}
	case "tags":
		// tagSource:aliyun(缺省)| platform。
		src, _ := cfgStr(ecsPowerTagSourceKey)
		switch strings.TrimSpace(src) {
		case "", "aliyun", "platform":
		default:
			return bad("tagSource must be \"aliyun\" or \"platform\"")
		}
		// tags:逗号分隔 k=v / k(与平台标签同一字符集口径,复用 runner 校验)。
		raw, ok := cfgStr(ecsPowerTagsKey)
		if !ok || strings.TrimSpace(raw) == "" {
			return bad("tags is required when selector=tags")
		}
		terms := 0
		for _, p := range strings.Split(raw, ",") {
			if strings.TrimSpace(p) == "" {
				continue
			}
			terms++
		}
		if terms == 0 {
			return bad("tags is required when selector=tags")
		}
		if terms > ecsPowerMaxTagTerms {
			return bad("tags exceeds %d terms", ecsPowerMaxTagTerms)
		}
	default:
		return bad("selector must be \"instances\" or \"tags\"")
	}

	// waitReachable:可选 toggle(true=开机到达 Running 后继续等 TCP 网络可达;仅 start)。
	rawReach, _ := cfgStr(ecsPowerWaitReachableKey)
	switch strings.TrimSpace(rawReach) {
	case "", "true", "false":
	default:
		return bad("waitReachable must be \"true\" or \"false\"")
	}
	if strings.TrimSpace(rawReach) == "true" {
		if action != "start" {
			return bad("waitReachable is only valid with action \"start\"")
		}
		// 等待可达共用 waitSeconds 预算,必须显式给正数。
		rawWait, _ := cfgStr(ecsPowerWaitSecondsKey)
		n, err := strconv.Atoi(strings.TrimSpace(rawWait))
		if err != nil || n < 1 || n > ecsPowerMaxWaitSeconds {
			return bad("waitReachable requires waitSeconds in 1..%d", ecsPowerMaxWaitSeconds)
		}
	}
	// reachablePort:可选探活端口(1..65535;缺省 22,实例已关联机器行时优先行上端口)。
	if rawPort, ok := cfgStr(ecsPowerReachablePortKey); ok && strings.TrimSpace(rawPort) != "" {
		n, err := strconv.Atoi(strings.TrimSpace(rawPort))
		if err != nil || n < 1 || n > ecsPowerMaxPort {
			return bad("reachablePort must be an integer in 1..%d", ecsPowerMaxPort)
		}
	}
	return nil
}
