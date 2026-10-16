package pipeline

import "testing"

// hasCode 报告 issues 中是否含指定 code(可选校验 severity)。
func hasCode(issues []Issue, code string) (Issue, bool) {
	for _, is := range issues {
		if is.Code == code {
			return is, true
		}
	}
	return Issue{}, false
}

// validBase 构造一份合法(无 error)的基线输入:源+构建+部署阶段、dockerfile 模型、
// push 事件开启、项目凭据 OK。
func validBase() ValidationInput {
	return ValidationInput{
		Stages: []Stage{
			{ID: "s1", Name: "源", Kind: KindSource, Jobs: []Job{{ID: "j1", Name: "git", Type: "git_source"}}},
			{ID: "s2", Name: "构建", Kind: KindBuild, Jobs: []Job{{ID: "j2", Name: "镜像", Type: "docker_build"}}},
			{ID: "s3", Name: "部署", Kind: KindDeploy, Jobs: []Job{{ID: "j3", Name: "上线", Type: "deploy"}}},
		},
		Build:               BuildConfig{Model: BuildModelDockerfile, DockerfilePath: "Dockerfile"},
		Events:              ValidationEvents{Push: true},
		ProjectCredentialOK: true,
	}
}

func TestValidateLegalConfigHasNoIssues(t *testing.T) {
	issues := Validate(validBase())
	if len(issues) != 0 {
		t.Fatalf("合法配置应无 issue, got %+v", issues)
	}
	if !Ready(issues) {
		t.Fatalf("合法配置应 ready")
	}
}

func TestValidateSourceStageMissing(t *testing.T) {
	in := validBase()
	in.Stages = in.Stages[1:] // 去掉源阶段
	issues := Validate(in)
	if _, ok := hasCode(issues, "source_stage_missing"); !ok {
		t.Fatalf("应报 source_stage_missing: %+v", issues)
	}
	if Ready(issues) {
		t.Fatalf("缺源阶段不应 ready")
	}
}

func TestValidateNoBuildOrDeploy(t *testing.T) {
	in := validBase()
	// 仅保留源阶段(无 build/deploy 任务)。
	in.Stages = []Stage{{ID: "s1", Name: "源", Kind: KindSource, Jobs: []Job{{ID: "j1", Name: "git", Type: "git_source"}}}}
	issues := Validate(in)
	is, ok := hasCode(issues, "no_build_or_deploy")
	if !ok {
		t.Fatalf("应报 no_build_or_deploy: %+v", issues)
	}
	if is.Severity != SeverityWarning {
		t.Fatalf("no_build_or_deploy 应为 warning, got %s", is.Severity)
	}
	if !Ready(issues) {
		t.Fatalf("仅 warning 仍应 ready")
	}
}

func TestValidateToolchainIncomplete(t *testing.T) {
	in := validBase()
	in.Build = BuildConfig{Model: BuildModelToolchain, Toolchain: Toolchain{Language: "node", Version: ""}}
	issues := Validate(in)
	is, ok := hasCode(issues, "toolchain_incomplete")
	if !ok {
		t.Fatalf("应报 toolchain_incomplete: %+v", issues)
	}
	if is.Severity != SeverityError {
		t.Fatalf("应为 error, got %s", is.Severity)
	}
	if Ready(issues) {
		t.Fatalf("toolchain 不全不应 ready")
	}
}

func TestValidateDockerfilePathDefault(t *testing.T) {
	in := validBase()
	in.Build = BuildConfig{Model: BuildModelDockerfile, DockerfilePath: ""}
	issues := Validate(in)
	is, ok := hasCode(issues, "dockerfile_path_default")
	if !ok {
		t.Fatalf("应报 dockerfile_path_default: %+v", issues)
	}
	if is.Severity != SeverityWarning {
		t.Fatalf("应为 warning, got %s", is.Severity)
	}
}

func TestValidateBuildVarCredentialMissing(t *testing.T) {
	in := validBase()
	in.BuildVars = []ValidationVar{
		{Key: "NPM_TOKEN", Secret: true, CredentialID: "dead", CredentialExists: false},
	}
	issues := Validate(in)
	is, ok := hasCode(issues, "credential_missing")
	if !ok {
		t.Fatalf("应报 credential_missing: %+v", issues)
	}
	if is.Scope != ScopeVars || is.Severity != SeverityError {
		t.Fatalf("credential_missing 应 error/vars, got %s/%s", is.Severity, is.Scope)
	}
}

func TestValidateManualOnly(t *testing.T) {
	in := validBase()
	in.Events = ValidationEvents{}
	issues := Validate(in)
	is, ok := hasCode(issues, "manual_only")
	if !ok {
		t.Fatalf("应报 manual_only: %+v", issues)
	}
	if is.Severity != SeverityInfo {
		t.Fatalf("manual_only 应为 info, got %s", is.Severity)
	}
	if !Ready(issues) {
		t.Fatalf("仅 info 应 ready")
	}
}

func TestValidateProjectCredentialMissing(t *testing.T) {
	in := validBase()
	in.ProjectCredentialOK = false
	issues := Validate(in)
	is, ok := hasCode(issues, "project_credential_missing")
	if !ok {
		t.Fatalf("应报 project_credential_missing: %+v", issues)
	}
	if is.Severity != SeverityError {
		t.Fatalf("应为 error, got %s", is.Severity)
	}
	if Ready(issues) {
		t.Fatalf("项目凭据缺失不应 ready")
	}
}

func TestReadyComputation(t *testing.T) {
	if Ready([]Issue{{Severity: SeverityError}}) {
		t.Fatalf("含 error 不应 ready")
	}
	if !Ready([]Issue{{Severity: SeverityWarning}, {Severity: SeverityInfo}}) {
		t.Fatalf("仅 warning/info 应 ready")
	}
	if !Ready(nil) {
		t.Fatalf("空 issues 应 ready")
	}
}
