package pipeline

import (
	"strings"
	"testing"
)

// ecsPowerSpec 用最小合法骨架包一个 ecs_power job 走完整 NormalizeSpec(含挂载的校验)。
func ecsPowerSpec(t *testing.T, config map[string]any) (Spec, error) {
	t.Helper()
	return NormalizeSpec(Spec{Stages: []Stage{
		{
			ID: "s1", Name: "源", Kind: KindSource,
			Jobs: []Job{{ID: "j1", Name: "拉代码", Type: "git_source"}},
		},
		{
			ID: "s2", Name: "运维", Kind: KindCustom,
			Jobs: []Job{
				{ID: "j2", Name: "开关机", Type: "ecs_power", Config: config},
			},
		},
	}})
}

func TestValidateEcsPowerDirectInstances(t *testing.T) {
	// 合法:直选实例 + 开机。
	spec, err := ecsPowerSpec(t, map[string]any{
		"action": "start", "accountId": "acc-1", "selector": "instances",
		"instanceIds": "i-1,i-2",
	})
	if err != nil {
		t.Fatalf("合法直选应通过: %v", err)
	}
	if spec.Stages[1].Jobs[0].Type != "ecs_power" {
		t.Fatalf("spec = %+v", spec)
	}

	// 缺 instanceIds → 拒绝。
	_, err = ecsPowerSpec(t, map[string]any{"action": "start", "accountId": "acc-1"})
	if err == nil || !strings.Contains(err.Error(), "instanceIds") {
		t.Fatalf("缺 instanceIds err = %v", err)
	}

	// 超 100 台 → 拒绝。
	ids := make([]string, 101)
	for i := range ids {
		ids[i] = "i-x"
	}
	joined := strings.Join(ids, ",")
	_, err = ecsPowerSpec(t, map[string]any{
		"action": "start", "accountId": "acc-1", "instanceIds": joined,
	})
	if err == nil || !strings.Contains(err.Error(), "100") {
		t.Fatalf("超上限 err = %v", err)
	}
}

func TestValidateEcsPowerTags(t *testing.T) {
	// 合法:阿里云标签 + 关机。
	if _, err := ecsPowerSpec(t, map[string]any{
		"action": "stop", "accountId": "acc-1", "selector": "tags",
		"tagSource": "aliyun", "tags": "env=staging,team",
	}); err != nil {
		t.Fatalf("合法标签应通过: %v", err)
	}

	// 合法:平台标签(tagSource 缺省 aliyun 之外的合法值)。
	if _, err := ecsPowerSpec(t, map[string]any{
		"action": "stop", "accountId": "acc-1", "selector": "tags",
		"tagSource": "platform", "tags": "env=staging",
	}); err != nil {
		t.Fatalf("平台标签应通过: %v", err)
	}

	cases := []struct {
		name   string
		config map[string]any
		want   string
	}{
		{"空标签", map[string]any{"action": "stop", "accountId": "acc-1", "selector": "tags", "tags": " "}, "tags"},
		{"非法来源", map[string]any{"action": "stop", "accountId": "acc-1", "selector": "tags", "tagSource": "both", "tags": "a=b"}, "tagSource"},
		{"非法选择方式", map[string]any{"action": "stop", "accountId": "acc-1", "selector": "region", "tags": "a=b"}, "selector"},
		{"非法动作", map[string]any{"action": "reboot", "accountId": "acc-1", "instanceIds": "i-1"}, "action"},
		{"缺账户", map[string]any{"action": "start", "instanceIds": "i-1"}, "accountId"},
	}
	for _, tc := range cases {
		_, err := ecsPowerSpec(t, tc.config)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want 含 %q", tc.name, err, tc.want)
		}
	}
}
