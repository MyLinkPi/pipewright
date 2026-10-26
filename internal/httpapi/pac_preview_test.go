package httpapi

import (
	"net/http"
	neturl "net/url"
	"testing"
)

const validPacYAML = `stages:
  - name: 源码
    kind: source
    jobs:
      - name: checkout
        type: source
  - name: 构建
    kind: build
    jobs:
      - name: compile
        type: script
        script:
          commands:
            - make build
      - name: package
        type: script
        script:
          commands:
            - make package
`

// TestPacPreviewValid 验证按默认分支拉取合法 .pipewright.yml → found=true valid=true + 阶段摘要。
func TestPacPreviewValid(t *testing.T) {
	url := makeBareSourceRepo(t, map[string]string{".pipewright.yml": validPacYAML})
	srv, client, csrf, projID := setupSourceServer(t, url)

	code, m, raw := getJSON(t, client, srv.URL+"/api/projects/"+projID+"/pac/preview", csrf)
	if code != http.StatusOK {
		t.Fatalf("preview status=%d: %s", code, raw)
	}
	if m["found"] != true || m["valid"] != true {
		t.Fatalf("应 found=true valid=true, got %s", raw)
	}
	if m["ref"] != "main" {
		t.Fatalf("ref 应默认默认分支 main, got %v", m["ref"])
	}
	if m["file"] != ".pipewright.yml" {
		t.Fatalf("file 应为 .pipewright.yml, got %v", m["file"])
	}
	if sc, _ := m["stageCount"].(float64); sc != 2 {
		t.Fatalf("stageCount 应为 2, got %v (%s)", m["stageCount"], raw)
	}
	stages, ok := m["stages"].([]any)
	if !ok || len(stages) != 2 {
		t.Fatalf("stages 应有 2 项, got %s", raw)
	}
	build, _ := stages[1].(map[string]any)
	if build["kind"] != "build" {
		t.Fatalf("第二阶段 kind 应为 build, got %v", build["kind"])
	}
	if jc, _ := build["jobCount"].(float64); jc != 2 {
		t.Fatalf("build 阶段 jobCount 应为 2, got %v", build["jobCount"])
	}
	if m["error"] != "" {
		t.Fatalf("合法配置 error 应为空, got %v", m["error"])
	}
}

// TestPacPreviewInvalid 验证非法 YAML → found=true valid=false error 非空 stages=[]。
func TestPacPreviewInvalid(t *testing.T) {
	url := makeBareSourceRepo(t, map[string]string{
		".pipewright.yml": "stages:\n  - name: x\n    kind: not_a_real_kind\n",
	})
	srv, client, csrf, projID := setupSourceServer(t, url)

	code, m, raw := getJSON(t, client, srv.URL+"/api/projects/"+projID+"/pac/preview", csrf)
	if code != http.StatusOK {
		t.Fatalf("preview status=%d: %s", code, raw)
	}
	if m["found"] != true || m["valid"] != false {
		t.Fatalf("非法 YAML 应 found=true valid=false, got %s", raw)
	}
	if es, _ := m["error"].(string); es == "" {
		t.Fatalf("非法 YAML error 应非空, got %s", raw)
	}
	if stages, ok := m["stages"].([]any); !ok || len(stages) != 0 {
		t.Fatalf("非法 YAML stages 应为 [], got %s", raw)
	}
}

// TestPacPreviewNotFound 验证仓库无 .pipewright.yml → found=false(不报错)。
func TestPacPreviewNotFound(t *testing.T) {
	url := makeBareSourceRepo(t, map[string]string{"README.md": "# hi\n"})
	srv, client, csrf, projID := setupSourceServer(t, url)

	code, m, raw := getJSON(t, client, srv.URL+"/api/projects/"+projID+"/pac/preview", csrf)
	if code != http.StatusOK {
		t.Fatalf("preview status=%d: %s", code, raw)
	}
	if m["found"] != false {
		t.Fatalf("无文件应 found=false, got %s", raw)
	}
}

// TestPacPreviewProjectNotFound 验证未知项目 → 404 project_not_found。
func TestPacPreviewProjectNotFound(t *testing.T) {
	url := makeBareSourceRepo(t, map[string]string{".pipewright.yml": validPacYAML})
	srv, client, csrf, _ := setupSourceServer(t, url)

	code, _, _ := getJSON(t, client, srv.URL+"/api/projects/no-such-id/pac/preview", csrf)
	if code != http.StatusNotFound {
		t.Fatalf("未知项目应 404, got %d", code)
	}
}

// ---- 可指定文件 + 变量声明同步(流水线即代码 · 变量)----

const varsPacYAML = `version: 1
variables:
  - name: DB_CONN
    description: 生产数据库连接字符串
    required: true
    secret: true
  - name: BASE_DOMAIN
    description: 服务注册基域
    default: internal.example.com
stages:
  - name: 源码
    kind: source
    jobs:
      - name: checkout
        type: source
`

// TestPacPreviewFileParamSyncsVariables 验证:file 查询参数临时指定文件 → 校验通过返回
// variables 声明,并把声明同步进 pipeline_settings(GET settings 可见,defs 只读元信息)。
func TestPacPreviewFileParamSyncsVariables(t *testing.T) {
	url := makeBareSourceRepo(t, map[string]string{"ci/pipeline.yml": varsPacYAML})
	srv, client, csrf, projID := setupSourceServer(t, url)

	code, m, raw := getJSON(t, client, srv.URL+"/api/projects/"+projID+"/pac/preview?file=ci/pipeline.yml", csrf)
	if code != http.StatusOK {
		t.Fatalf("preview status=%d: %s", code, raw)
	}
	if m["found"] != true || m["valid"] != true || m["file"] != "ci/pipeline.yml" {
		t.Fatalf("应命中指定文件, got %s", raw)
	}
	vars, ok := m["variables"].([]any)
	if !ok || len(vars) != 2 {
		t.Fatalf("variables 应有 2 条声明, got %s", raw)
	}
	dbVar, _ := vars[0].(map[string]any)
	if dbVar["name"] != "DB_CONN" || dbVar["required"] != true || dbVar["secret"] != true {
		t.Fatalf("DB_CONN 声明不符: %v", dbVar)
	}

	// 声明已同步进 pipeline_settings(「变量与缓存」数据源)。
	code, m, raw = getJSON(t, client, srv.URL+"/api/projects/"+projID+"/pipeline/settings", csrf)
	if code != http.StatusOK {
		t.Fatalf("settings status=%d: %s", code, raw)
	}
	pv, _ := m["pacVars"].(map[string]any)
	if pv == nil {
		t.Fatalf("settings 应含 pacVars, got %s", raw)
	}
	defs, _ := pv["defs"].([]any)
	if len(defs) != 2 {
		t.Fatalf("pacVars.defs 应有 2 条, got %s", raw)
	}
	if pv["ref"] != "main" || pv["file"] != "ci/pipeline.yml" {
		t.Fatalf("同步来源元信息不符: %v", pv)
	}
}

// TestPacPreviewProjectPacFile 验证项目 pac_file 配置生效:预览默认读该文件,
// 仓库没有 .pipewright.yml 也能命中自定义路径。
func TestPacPreviewProjectPacFile(t *testing.T) {
	url := makeBareSourceRepo(t, map[string]string{"ci/pipeline.yml": validPacYAML})
	srv, client, csrf, projID := setupSourceServer(t, url, "ci/pipeline.yml")

	code, m, raw := getJSON(t, client, srv.URL+"/api/projects/"+projID+"/pac/preview", csrf)
	if code != http.StatusOK {
		t.Fatalf("preview status=%d: %s", code, raw)
	}
	if m["found"] != true || m["valid"] != true || m["file"] != "ci/pipeline.yml" {
		t.Fatalf("应按项目 pac_file 命中, got %s", raw)
	}
}

// TestPacPreviewInvalidFileParam 验证 file 查询参数非法(绝对路径/.. 段)→ 400。
func TestPacPreviewInvalidFileParam(t *testing.T) {
	url := makeBareSourceRepo(t, map[string]string{".pipewright.yml": validPacYAML})
	srv, client, csrf, projID := setupSourceServer(t, url)

	for _, bad := range []string{"../etc/passwd", "/abs/p.yml", `C:\x\y.yml`} {
		code, _, _ := getJSON(t, client, srv.URL+"/api/projects/"+projID+"/pac/preview?file="+neturl.QueryEscape(bad), csrf)
		if code != http.StatusBadRequest {
			t.Fatalf("非法 file %q 应 400, got %d", bad, code)
		}
	}
}

// TestPacPreviewInvalidYAMLNoSync 验证校验失败不污染已同步的声明。
func TestPacPreviewInvalidYAMLNoSync(t *testing.T) {
	url := makeBareSourceRepo(t, map[string]string{
		".pipewright.yml": "stages:\n  - name: x\n    kind: not_a_real_kind\n",
	})
	srv, client, csrf, projID := setupSourceServer(t, url)

	if _, m, raw := getJSON(t, client, srv.URL+"/api/projects/"+projID+"/pac/preview", csrf); m["valid"] != false {
		t.Fatalf("应 valid=false, got %s", raw)
	}
	_, m, raw := getJSON(t, client, srv.URL+"/api/projects/"+projID+"/pipeline/settings", csrf)
	pv, _ := m["pacVars"].(map[string]any)
	if pv != nil {
		if defs, _ := pv["defs"].([]any); len(defs) != 0 {
			t.Fatalf("校验失败不应同步声明, got %s", raw)
		}
	}
}
