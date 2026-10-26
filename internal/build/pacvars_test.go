package build

import (
	"context"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/pipeline"
	"github.com/huangchengsir/pipewright/internal/run"
)

// pacVarsFixture 是一组典型声明:required secret / 带默认值 / 普通变量 / required 未填。
func pacVarsFixture() pipeline.PacVarsConfig {
	return pipeline.PacVarsConfig{
		Defs: []pipeline.PacVarDef{
			{Name: "DB_CONN", Description: "数据库连接串", Required: true, Secret: true},
			{Name: "BASE_DOMAIN", Description: "基域", Default: "internal.example.com"},
			{Name: "APP_SUB", Description: "子域名"},
			{Name: "MUST_FILL", Required: true},
		},
		Values: []pipeline.BuildVar{
			{Key: "DB_CONN", Secret: true, CredentialID: "cred-1"},
			{Key: "APP_SUB", Value: "billing"},
		},
	}
}

func TestResolvePacVarValues(t *testing.T) {
	reveal := func(credID string) string {
		if credID == "cred-1" {
			return "postgres://u:p@db:5432/x"
		}
		return "" // 其它凭据视为取不到
	}
	values, unresolved := ResolvePacVarValues(pacVarsFixture(), reveal)

	if values["DB_CONN"] != "postgres://u:p@db:5432/x" {
		t.Fatalf("secret 值应经 reveal 解析, got %q", values["DB_CONN"])
	}
	if values["APP_SUB"] != "billing" {
		t.Fatalf("明文值直取, got %q", values["APP_SUB"])
	}
	if values["BASE_DOMAIN"] != "internal.example.com" {
		t.Fatalf("未填值应回 default, got %q", values["BASE_DOMAIN"])
	}
	if strings.Join(unresolved, ",") != "MUST_FILL" {
		t.Fatalf("未填的 required 变量应列入 unresolved, got %v", unresolved)
	}

	// secret reveal 失败 → 未解析(default 兜底不适用 secret)。
	pv := pipeline.PacVarsConfig{
		Defs:   []pipeline.PacVarDef{{Name: "S", Secret: true}},
		Values: []pipeline.BuildVar{{Key: "S", Secret: true, CredentialID: "gone"}},
	}
	if _, unresolved := ResolvePacVarValues(pv, reveal); strings.Join(unresolved, ",") != "S" {
		t.Fatalf("reveal 失败应列入 unresolved, got %v", unresolved)
	}
}

func TestMissingRequiredPacVars(t *testing.T) {
	miss := MissingRequiredPacVars(pacVarsFixture().Defs, []string{"MUST_FILL", "APP_SUB"})
	if strings.Join(miss, ",") != "MUST_FILL" {
		t.Fatalf("只有 required 且未解析的才缺失, got %v", miss)
	}
	if got := MissingRequiredPacVars(pacVarsFixture().Defs, []string{"APP_SUB"}); got != nil {
		t.Fatalf("非 required 未填不算缺失, got %v", got)
	}
}

func TestApplyPacVarsToStage(t *testing.T) {
	values := map[string]string{
		"DB_CONN":     "postgres://u:p@db:5432/x",
		"BASE_DOMAIN": "internal.example.com",
		"APP_SUB":     "billing",
	}
	stage := pipeline.Stage{
		Jobs: []pipeline.Job{
			{
				Name: "部署",
				Type: "deploy_ssh",
				Config: map[string]any{
					"domain":    "${APP_SUB}.${BASE_DOMAIN}",
					"dbUrl":     "${DB_CONN}",
					"untouched": "plain",
					"unknown":   "${NOT_DECLARED}",
				},
			},
		},
		Services: []pipeline.ServiceSpec{{Name: "redis", Image: "redis:7", Env: []string{"ADDR=${BASE_DOMAIN}:6379"}}},
		Post:     []pipeline.PostStep{{Condition: "always", Commands: []string{"echo ${APP_SUB}"}}},
	}

	ApplyPacVarsToStage(&stage, values)

	cfg := stage.Jobs[0].Config
	if cfg["domain"] != "billing.internal.example.com" {
		t.Fatalf("组合占位应整体替换, got %v", cfg["domain"])
	}
	if cfg["dbUrl"] != "postgres://u:p@db:5432/x" {
		t.Fatalf("secret 值应替换, got %v", cfg["dbUrl"])
	}
	if cfg["untouched"] != "plain" || cfg["unknown"] != "${NOT_DECLARED}" {
		t.Fatalf("无占位/未声明占位应原样保留, got %v", cfg)
	}
	if stage.Services[0].Env[0] != "ADDR=internal.example.com:6379" {
		t.Fatalf("services env 应替换, got %v", stage.Services[0].Env[0])
	}
	if stage.Post[0].Commands[0] != "echo billing" {
		t.Fatalf("post commands 应替换, got %v", stage.Post[0].Commands[0])
	}

	// 幂等:再跑一次不递归展开(值里含 ${X} 也不二次替换)。
	values["APP_SUB"] = "${BASE_DOMAIN}"
	ApplyPacVarsToStage(&stage, values)
	if cfg["unknown"] != "${NOT_DECLARED}" {
		t.Fatalf("未声明占位应恒原样, got %v", cfg["unknown"])
	}
}

func TestApplyStagePacVarsGates(t *testing.T) {
	b := &Builder{} // 零依赖:门控路径不应触碰 resolve
	stage := pipeline.Stage{Jobs: []pipeline.Job{{
		Name: "x", Type: "deploy_ssh", Config: map[string]any{"domain": "${BASE_DOMAIN}"},
	}}}

	// 非 repo 来源(库内画布配置)恒不替换、不拦截。
	r := &run.Run{SpecSource: run.SpecSource{Source: run.SpecSourceStored}}
	if miss := b.applyStagePacVars(context.Background(), r, &stage); miss != nil {
		t.Fatalf("stored 来源不应拦截, got %v", miss)
	}
	if stage.Jobs[0].Config["domain"] != "${BASE_DOMAIN}" {
		t.Fatalf("stored 来源不应替换, got %v", stage.Jobs[0].Config["domain"])
	}

	// repo 来源但依赖未装配(resolve 失败):优雅放行,占位保留(不中断运行)。
	r2 := &run.Run{SpecSource: run.SpecSource{Source: run.SpecSourceRepo}}
	if miss := b.applyStagePacVars(context.Background(), r2, &stage); miss != nil {
		t.Fatalf("依赖缺失应放行, got %v", miss)
	}
	if stage.Jobs[0].Config["domain"] != "${BASE_DOMAIN}" {
		t.Fatalf("依赖缺失不应替换, got %v", stage.Jobs[0].Config["domain"])
	}
}
