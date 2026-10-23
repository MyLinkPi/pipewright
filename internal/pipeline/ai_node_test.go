package pipeline

// ai_node_test.go 覆盖 ai_test 节点保存期校验:onAiUnavailable 枚举、timeout/maxSteps
// 范围、model/modelContextTokens 成对、deployJob 同阶段引用(specs/ai-test-node.md)。

import (
	"strings"
	"testing"
)

func aiTestStage(jobs ...Job) []Stage {
	return []Stage{{ID: "s1", Name: "验收", Kind: KindDeploy, Jobs: jobs}}
}

func aiTestJobCfg(cfg map[string]any) Job {
	if cfg == nil {
		cfg = map[string]any{}
	}
	return Job{ID: "a1", Name: "ai", Type: "ai_test", Config: cfg}
}

func TestValidateAITestAcceptsDefaults(t *testing.T) {
	if err := validateAITest(aiTestStage(aiTestJobCfg(nil))); err != nil {
		t.Fatalf("缺省配置应通过,得到:%v", err)
	}
}

func TestValidateAITestOnUnavailableEnum(t *testing.T) {
	for _, v := range []string{"fail", "skip", ""} {
		if err := validateAITest(aiTestStage(aiTestJobCfg(map[string]any{"onAiUnavailable": v}))); err != nil {
			t.Fatalf("onAiUnavailable=%q 应通过:%v", v, err)
		}
	}
	if err := validateAITest(aiTestStage(aiTestJobCfg(map[string]any{"onAiUnavailable": "ignore"}))); err == nil {
		t.Fatal("onAiUnavailable=ignore 应被拒")
	}
}

func TestValidateAITestRanges(t *testing.T) {
	if err := validateAITest(aiTestStage(aiTestJobCfg(map[string]any{"timeoutSeconds": 3600}))); err != nil {
		t.Fatalf("timeout=3600 应通过:%v", err)
	}
	if err := validateAITest(aiTestStage(aiTestJobCfg(map[string]any{"timeoutSeconds": 3601}))); err == nil {
		t.Fatal("timeout=3601 应被拒")
	}
	if err := validateAITest(aiTestStage(aiTestJobCfg(map[string]any{"maxSteps": 100}))); err != nil {
		t.Fatalf("maxSteps=100 应通过:%v", err)
	}
	if err := validateAITest(aiTestStage(aiTestJobCfg(map[string]any{"maxSteps": 101}))); err == nil {
		t.Fatal("maxSteps=101 应被拒")
	}
}

func TestValidateAITestStrictInts(t *testing.T) {
	// 非法整数显式报错(不静默当未配置):Sscanf 时代的 "123abc" 前缀解析被拒。
	for _, v := range []any{"123abc", "abc", 1.5, true} {
		if err := validateAITest(aiTestStage(aiTestJobCfg(map[string]any{"timeoutSeconds": v}))); err == nil {
			t.Fatalf("timeoutSeconds=%v 应被拒", v)
		}
	}
	// 合法形态:整数字符串 / int / 整数值 float64。
	for _, v := range []any{"300", 300, 300.0} {
		if err := validateAITest(aiTestStage(aiTestJobCfg(map[string]any{"timeoutSeconds": v}))); err != nil {
			t.Fatalf("timeoutSeconds=%v 应通过:%v", v, err)
		}
	}
	// 空串 = 未配置。
	if err := validateAITest(aiTestStage(aiTestJobCfg(map[string]any{"timeoutSeconds": "  "}))); err != nil {
		t.Fatalf("空串应视为未配置:%v", err)
	}
	// filter:非字符串类型拒;数字字符串是合法 glob 不拒。
	if err := validateAITest(aiTestStage(aiTestJobCfg(map[string]any{"filter": 123}))); err == nil {
		t.Fatal("filter 数字类型应被拒")
	}
	if err := validateAITest(aiTestStage(aiTestJobCfg(map[string]any{"filter": "0"}))); err != nil {
		t.Fatalf("数字字符串 filter 应通过:%v", err)
	}
	// model 非字符串 → 拒(无论是否配窗口)。
	if err := validateAITest(aiTestStage(aiTestJobCfg(map[string]any{"model": 42}))); err == nil {
		t.Fatal("model 非字符串应被拒")
	}
	if err := validateAITest(aiTestStage(aiTestJobCfg(map[string]any{"model": 42, "modelContextTokens": 128000}))); err == nil {
		t.Fatal("model 非字符串(带窗口)应被拒")
	}
	// modelContextTokens 非法整数 → 拒(而非按「未成对」误导)。
	if err := validateAITest(aiTestStage(aiTestJobCfg(map[string]any{"model": "gpt-x", "modelContextTokens": "abc"}))); err == nil {
		t.Fatal("modelContextTokens 非法整数应被拒")
	}
}

func TestValidateAITestModelContextPairing(t *testing.T) {
	// 只有 model 无窗口 → 拒。
	if err := validateAITest(aiTestStage(aiTestJobCfg(map[string]any{"model": "gpt-x"}))); err == nil {
		t.Fatal("model 无 modelContextTokens 应被拒")
	}
	// 只有窗口无 model → 拒。
	if err := validateAITest(aiTestStage(aiTestJobCfg(map[string]any{"modelContextTokens": 128000}))); err == nil {
		t.Fatal("modelContextTokens 无 model 应被拒")
	}
	// 成对 + 合法范围 → 过。
	if err := validateAITest(aiTestStage(aiTestJobCfg(map[string]any{"model": "gpt-x", "modelContextTokens": 128000}))); err != nil {
		t.Fatalf("成对合法应通过:%v", err)
	}
	// 范围外 → 拒(显式报错,不静默回默认)。
	for _, v := range []any{999, 2000001} {
		if err := validateAITest(aiTestStage(aiTestJobCfg(map[string]any{"model": "gpt-x", "modelContextTokens": v}))); err == nil {
			t.Fatalf("modelContextTokens=%v 应被拒", v)
		}
	}
}

func TestValidateAITestDeployJobSameStage(t *testing.T) {
	deploy := Job{ID: "d1", Name: "部署", Type: "deploy_ssh", Config: map[string]any{}}
	ai := aiTestJobCfg(map[string]any{"deployJob": "部署"})
	// 同阶段 → 过。
	if err := validateAITest([]Stage{{ID: "s1", Name: "验收", Kind: KindDeploy, Jobs: []Job{deploy, ai}}}); err != nil {
		t.Fatalf("同阶段 deployJob 应通过:%v", err)
	}
	// 跨阶段 → 拒(与 push_image 的 artifactJob 同款约束)。
	cross := []Stage{
		{ID: "s0", Name: "部署阶段", Kind: KindDeploy, Jobs: []Job{deploy}},
		{ID: "s1", Name: "验收", Kind: KindDeploy, Jobs: []Job{ai}},
	}
	if err := validateAITest(cross); err == nil || !strings.Contains(err.Error(), "同阶段") {
		t.Fatalf("跨阶段 deployJob 应被拒(同阶段提示),得到:%v", err)
	}
	// 引用不存在 → 拒。
	if err := validateAITest(aiTestStage(aiTestJobCfg(map[string]any{"deployJob": "不存在"}))); err == nil {
		t.Fatal("deployJob 引用不存在应被拒")
	}
	// 引用非部署类节点 → 拒。
	other := Job{ID: "n1", Name: "脚本", Type: "script", Config: map[string]any{}}
	if err := validateAITest([]Stage{{ID: "s1", Name: "验收", Kind: KindDeploy, Jobs: []Job{other, aiTestJobCfg(map[string]any{"deployJob": "脚本"})}}}); err == nil {
		t.Fatal("deployJob 引用非部署节点应被拒")
	}
}
