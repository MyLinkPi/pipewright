package pipeline

import (
	"fmt"
	"strings"
)

// ai_node.go 校验 AI 测试节点(ai_test)的配置形态(specs/ai-test-node.md)。
//
// ai_test 在 run 中为每条仓库需求(.pipewright/tests/requirements/**.md)启动一个
// agent 会话:理解需求 → 驱动浏览器/HTTP 按需求操作被测系统 → 观察实际结果 →
// 输出结构化裁决。裁决一律 advisory,放行决策归人工审批门;节点仅因基础设施
// 错误失败(发现 0 需求/解析失败/AI 不可达且 onAiUnavailable=fail)。
//
// 保存期只校验形态(枚举/范围/成对/引用),不触库不触网:需求路径存在性与文件
// 合法性由运行时发现步骤裁决(保存期只看 spec JSON,看不见仓库)。
const (
	AITestType = "ai_test"

	aiTestFilterKey        = "filter"
	aiTestOnUnavailableKey = "onAiUnavailable"
	aiTestTimeoutKey       = "timeoutSeconds"
	aiTestMaxStepsKey      = "maxSteps"
	aiTestModelKey         = "model"
	aiTestModelCtxKey      = "modelContextTokens"
	aiTestDeployJobKey     = "deployJob"

	aiTestDefaultFilter = ".pipewright/tests/requirements/**/*.md"
	aiTestMinModelCtx   = 1_000
	aiTestMaxModelCtx   = 2_000_000
	aiTestMaxTimeout    = 3600
	aiTestMaxSteps      = 100
)

// aiTestDeployTypes 是 deployJob 可引用的部署类节点(与 build.isDeployJob 同一集合)。
var aiTestDeployTypes = map[string]bool{
	"deploy_ssh":       true,
	"deploy_container": true,
	"deploy_static":    true,
	"deploy_frontend":  true, // legacy
}

// validateAITest 校验全部 ai_test 节点(错误归一为 ErrInvalidJob)。
func validateAITest(stages []Stage) error {
	// 名字 → 节点索引(deployJob 引用解析用;spec 内名字唯一由通用校验保证)。
	byName := map[string]Job{}
	for si := range stages {
		for ji := range stages[si].Jobs {
			n := stages[si].Jobs[ji]
			byName[n.Name] = n
		}
	}
	for si := range stages {
		for ji := range stages[si].Jobs {
			n := stages[si].Jobs[ji]
			if strings.TrimSpace(n.Type) != AITestType {
				continue
			}
			if err := validateAITestJob(n, byName, stages); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateAITestJob(n Job, byName map[string]Job, stages []Stage) error {
	cfgStr := func(key string) string {
		if raw, ok := n.Config[key]; ok && raw != nil {
			if s, isStr := raw.(string); isStr {
				return strings.TrimSpace(s)
			}
		}
		return ""
	}
	cfgInt := func(key string) (int, bool) {
		switch v := n.Config[key].(type) {
		case string:
			var out int
			if _, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &out); err == nil {
				return out, true
			}
			return 0, false
		case float64:
			return int(v), true
		case int:
			return v, true
		}
		return 0, false
	}
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: ai_test job %q: "+format, append([]any{ErrInvalidJob, n.Name}, args...)...)
	}

	if f := cfgStr(aiTestFilterKey); f != "" && strings.Contains(f, "\x00") {
		return bad("filter 含非法字符")
	}
	if v, ok := cfgInt(aiTestFilterKey); ok && v != 0 {
		return bad("filter 应为 glob 字符串")
	}

	switch u := cfgStr(aiTestOnUnavailableKey); u {
	case "", "fail", "skip":
	default:
		return bad("onAiUnavailable 仅允许 fail/skip(得到 %q)", u)
	}

	if t, ok := cfgInt(aiTestTimeoutKey); ok && (t < 0 || t > aiTestMaxTimeout) {
		return bad("timeoutSeconds 范围 0–%d", aiTestMaxTimeout)
	}
	if s, ok := cfgInt(aiTestMaxStepsKey); ok && (s < 0 || s > aiTestMaxSteps) {
		return bad("maxSteps 范围 0–%d", aiTestMaxSteps)
	}

	// model / modelContextTokens 成对:覆盖模型必须同时声明其上下文窗口
	// (压缩时机依赖;与 internal/ai normalizeContext 同边界,但显式报错不静默回默认)。
	model, hasModel := n.Config[aiTestModelKey]
	modelCtx, hasCtx := cfgInt(aiTestModelCtxKey)
	modelStr := ""
	if raw, ok := model.(string); ok {
		modelStr = strings.TrimSpace(raw)
	}
	if modelStr == "" {
		if hasCtx {
			return bad("modelContextTokens 需与 model 成对出现(未覆盖模型)")
		}
	} else {
		if _, ok := model.(string); !ok || !hasModel {
			return bad("model 应为字符串")
		}
		if !hasCtx {
			return bad("覆盖 model 时必须同时提供 modelContextTokens(该模型的上下文窗口 tokens)")
		}
		if modelCtx < aiTestMinModelCtx || modelCtx > aiTestMaxModelCtx {
			return bad("modelContextTokens 范围 %d–%d", aiTestMinModelCtx, aiTestMaxModelCtx)
		}
	}

	// deployJob:可选;必须引用**同阶段**的部署类节点(与 push_image 的 artifactJob 同款
	// 约束——运行期执行器按阶段分发,同阶段引用才能在执行时解析到目标机)。
	if ref := cfgStr(aiTestDeployJobKey); ref != "" {
		target, ok := byName[ref]
		if !ok {
			return bad("deployJob %q 在流水线中不存在", ref)
		}
		if !aiTestDeployTypes[strings.TrimSpace(target.Type)] {
			return bad("deployJob %q 不是部署类节点(得到 %q)", ref, target.Type)
		}
		if !aiTestSameStage(stages, n, target) {
			return bad("deployJob %q 必须与 ai_test 同阶段(引用同阶段部署节点,如 push_image 的 artifactJob)", ref)
		}
	}
	return nil
}

// aiTestSameStage 判断两节点是否在同一阶段。
func aiTestSameStage(stages []Stage, a, b Job) bool {
	for si := range stages {
		hasA, hasB := false, false
		for ji := range stages[si].Jobs {
			if stages[si].Jobs[ji].ID == a.ID {
				hasA = true
			}
			if stages[si].Jobs[ji].ID == b.ID {
				hasB = true
			}
		}
		if hasA && hasB {
			return true
		}
		if hasA || hasB {
			return false
		}
	}
	return false
}
