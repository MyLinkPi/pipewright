package httpapi

// 旧版部署「重新部署」的参数反推契约测试:extractLegacyParams(模板 compose 占位符锚文本
// ↔ 旧渲染产物)纯函数语义 + upgrade-context 无边车时的 legacy 分支(found 仍 false,
// legacy:true + 反推参数;旧 compose 也读不到 → 裸 found:false)。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/appstore"
	"github.com/huangchengsir/pipewright/internal/target"
)

func TestExtractLegacyParams(t *testing.T) {
	tplCompose := strings.Join([]string{
		"services:",
		"  gitea:",
		"    environment:",
		`      GITEA__server__SSH_DOMAIN: "{{ssh_domain}}"`,
		`      GITEA__server__HTTP_PORT: "{{port}}"`,
		`      GITEA__database__PASSWD: "{{password}}"`,
		"    command: --domain {{ssh_domain}} --keep {{keep}}",
	}, "\n")
	oldCompose := strings.Join([]string{
		"services:",
		"  gitea:",
		"    environment:",
		`      GITEA__server__SSH_DOMAIN: "git.example.com"`,
		`      GITEA__server__HTTP_PORT: "13000"`,
		`      GITEA__database__PASSWD: "s3cr3t!"`,
		"    command: --domain git.example.com --keep true",
	}, "\n")
	specs := []appstore.ParamSpec{
		{Name: "ssh_domain", Type: appstore.ParamTypeString, Required: true},
		{Name: "port", Type: appstore.ParamTypeInt},
		{Name: "password", Type: appstore.ParamTypeSecret, AutoGenerate: true},
		{Name: "keep", Type: appstore.ParamTypeString},
	}

	got := extractLegacyParams(tplCompose, oldCompose, specs)
	// ssh_domain 两处出现一致 → 采纳;port/keep 正常提取;secret 也反推(保持原口令部署)。
	if got["ssh_domain"] != "git.example.com" {
		t.Fatalf("ssh_domain 反推失败: %+v", got)
	}
	if got["port"] != "13000" || got["keep"] != "true" {
		t.Fatalf("port/keep 反推失败: %+v", got)
	}
	if got["password"] != "s3cr3t!" {
		t.Fatalf("secret 应反推以保持原口令: %+v", got)
	}

	// 两处出现值不一致 → 整个参数跳过(不可靠)。
	bad := strings.Replace(oldCompose, "command: --domain git.example.com", "command: --domain other.example.com", 1)
	if got = extractLegacyParams(tplCompose, bad, specs); len(got) != 0 {
		if _, ok := got["ssh_domain"]; ok {
			t.Fatalf("出现处不一致应跳过 ssh_domain: %+v", got)
		}
	}

	// 旧文本里找不到前锚 → 零反推。
	if got = extractLegacyParams(tplCompose, "完全无关的内容", specs); len(got) != 0 {
		t.Fatalf("无锚可定位应零反推: %+v", got)
	}
}

func TestExtractLegacyParamsEdge(t *testing.T) {
	// 占位符嵌在字符串中间,前锚 `https://` 后锚 `/x"` → 提取 a.b.c。
	got := extractLegacyParams(`URL: "https://{{host}}/x"`, `URL: "https://a.b.c/x"`,
		[]appstore.ParamSpec{{Name: "host", Type: appstore.ParamTypeString}})
	if got["host"] != "a.b.c" {
		t.Fatalf("嵌串占位符反推失败: %+v", got)
	}

	// 占位符在行尾(前锚有、后锚空)→ 取到行尾。
	got = extractLegacyParams("CMD: {{mode}}\n", "CMD: strict\nNEXT: x\n",
		[]appstore.ParamSpec{{Name: "mode", Type: appstore.ParamTypeString}})
	if got["mode"] != "strict" {
		t.Fatalf("行尾占位符反推失败: %+v", got)
	}

	// 行首占位符(无前锚)→ 跳过(定位不可靠)。
	if got = extractLegacyParams("{{solo}}: 1\n", "abc: 1\n",
		[]appstore.ParamSpec{{Name: "solo", Type: appstore.ParamTypeString}}); len(got) != 0 {
		t.Fatalf("无前锚应跳过: %+v", got)
	}
}

// TestUpgradeContextLegacy 无边车 + 栈目录旧 compose → found:false + legacy:true + 反推参数
// (供「重新部署」预填);compose 也读不到 → found:false 且无 legacy。
func TestUpgradeContextLegacy(t *testing.T) {
	apps := &deployAppFake{
		tpl: &appstore.Template{
			ID: "t1", Name: "gitea", DisplayName: "Gitea",
			ComposeYAML: "services:\n  g:\n    environment:\n      SSH_DOMAIN: \"{{ssh_domain}}\"\n",
			Params:      []appstore.ParamSpec{{Name: "ssh_domain", Type: appstore.ParamTypeString, Required: true}},
		},
		rendered: &appstore.RenderResult{Compose: "x"},
	}
	srv := &target.Server{ID: "srv-1", Name: "build-1", Host: "192.168.1.10"}

	r := upgradeRouter(apps, &deployTargetFake{srv: srv, artifacts: map[string]string{
		stacksBaseDir + "/gitea/" + composeFileName: "services:\n  g:\n    environment:\n      SSH_DOMAIN: \"10.0.0.8\"\n",
	}})
	var out struct {
		Found  bool              `json:"found"`
		Legacy bool              `json:"legacy"`
		Params map[string]string `json:"params"`
	}
	req := httptest.NewRequest(http.MethodGet, "/servers/srv-1/apps/upgrade-context?templateId=t1", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("解码: %v", err)
	}
	if out.Found || !out.Legacy || out.Params["ssh_domain"] != "10.0.0.8" {
		t.Fatalf("应 found:false + legacy:true + 反推 ssh_domain: %+v", out)
	}

	// 旧 compose 也读不到 → 裸 found:false(现状),无 legacy。
	r2 := upgradeRouter(apps, &deployTargetFake{srv: srv})
	req2 := httptest.NewRequest(http.MethodGet, "/servers/srv-1/apps/upgrade-context?templateId=t1", nil)
	rec2 := httptest.NewRecorder()
	r2.ServeHTTP(rec2, req2)
	var out2 struct {
		Found  bool `json:"found"`
		Legacy bool `json:"legacy"`
	}
	if err := json.NewDecoder(rec2.Body).Decode(&out2); err != nil {
		t.Fatalf("解码: %v", err)
	}
	if out2.Found || out2.Legacy {
		t.Fatalf("无 compose 应裸 found:false: %+v", out2)
	}
}
