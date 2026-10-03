package deploy

// selector_targets.go 是「部署目标圈选」层:把部署请求里的目标说明(serverIDs 或 selector)解析成
// 具体机器列表。选择器语法**完整复用构建机池**(internal/runner/selector.go 纯函数层):
// 空串 = 无目标;`server:<id>` = 钉单机;`linux,arch=arm64` = 逗号分隔 AND 标签项,在
// servers.labels 打了标签的机器里圈选。部署与构建共用同一套标签语义,机器打一次标签两边可用。
//
// 核心语义(与旧「人工勾选 serverIds」的差异):
//   - 显式 serverIDs 优先(环境回滚历史回放 / 显式 API 调用):存在性从严,任一不存在整次拒绝。
//   - 选择器空、或非空但零命中(含 `server:<id>` 指向已删除的机器)→ (nil, nil):
//     调用方按「跳过即成功」处理——自动化链路不因未配目标中断,也不静默写半截部署记录。
//   - 选择器语法非法 → ErrInvalidSelector(客户端错误,HTTP 层 422)。

import (
	"context"
	"errors"
	"strings"

	"github.com/huangchengsir/pipewright/internal/runner"
	"github.com/huangchengsir/pipewright/internal/target"
)

// ErrInvalidSelector 表示部署目标选择器语法非法(供 HTTP 层映射 422)。
var ErrInvalidSelector = errors.New("deploy: invalid target selector")

// 标签匹配方式(Config["selectorMode"] / DeployInput.SelectorMode):空 / 未知 → all(存量语义)。
const (
	// SelectorModeAll 满足全部标签项才命中(且;默认,与既有逗号语义一致)。
	SelectorModeAll = "all"
	// SelectorModeAny 满足任一标签项即命中(或)。
	SelectorModeAny = "any"
)

// normalizeSelectorMode 归一匹配方式(空 / 未知 → all,保持存量 AND 语义)。
func normalizeSelectorMode(s string) string {
	if strings.TrimSpace(strings.ToLower(s)) == SelectorModeAny {
		return SelectorModeAny
	}
	return SelectorModeAll
}

// resolveTargets 解析部署目标(见文件头语义)。servers 为 nil 且 err 为 nil = 无目标(跳过即成功)。
func (s *service) resolveTargets(ctx context.Context, serverIDs []string, selector, selectorMode string) ([]*target.Server, error) {
	// 显式机器列表优先:逐台解析,任一不存在 → ErrServerNotFound(整次拒绝,不留半截)。
	if len(serverIDs) > 0 {
		servers := make([]*target.Server, 0, len(serverIDs))
		for _, sid := range serverIDs {
			srv, err := s.targets.Get(ctx, sid)
			if err != nil {
				if errors.Is(err, target.ErrNotFound) {
					return nil, ErrServerNotFound
				}
				return nil, err
			}
			servers = append(servers, srv)
		}
		return servers, nil
	}

	selector = strings.TrimSpace(selector)
	if selector == "" {
		return nil, nil // 未配目标:跳过即成功。
	}
	// `server:<id>` 钉单机:机器已删视作零命中 → 跳过(与标签零命中同语义,不区别对待)。
	if id, ok := runner.PinnedServer(selector); ok {
		srv, err := s.targets.Get(ctx, id)
		switch {
		case errors.Is(err, target.ErrNotFound):
			return nil, nil
		case err != nil:
			return nil, err
		}
		return []*target.Server{srv}, nil
	}
	// 标签选择器:语法复用构建机池纯函数,全表取机后按 selectorMode 匹配
	// (all = 全部项命中(且,默认 = 存量语义);any = 任一项命中(或))。
	terms, err := runner.ParseSelector(selector)
	if err != nil {
		return nil, ErrInvalidSelector
	}
	all, err := s.targets.List(ctx)
	if err != nil {
		return nil, err
	}
	match := runner.MatchSelector
	if selectorMode == SelectorModeAny {
		match = runner.MatchSelectorAny
	}
	servers := make([]*target.Server, 0, 4)
	for _, srv := range all {
		if match(srv.Labels, terms) {
			servers = append(servers, srv)
		}
	}
	if len(servers) == 0 {
		return nil, nil // 零命中:跳过即成功(标签配错不阻断链路,由调用方在结果里明示「已跳过」)。
	}
	return servers, nil
}
