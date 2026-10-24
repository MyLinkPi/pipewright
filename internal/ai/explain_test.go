package ai

// 解读 prompt 的纯函数测试:字段行注入、镜像引用只留末段(内部 registry 不出网)、
// 搜索参考资料段、超长字段截断。

import (
	"strings"
	"testing"
)

func TestBuildFindingExplainPrompt(t *testing.T) {
	in := ExplainFindingInput{
		ScanType: "trivy", Source: "image:registry.internal:5000/team/app:2.0",
		Severity: "high", VulnID: "CVE-2024-1234", Title: "something",
		Target: "openssl", InstalledVersion: "1.1.1c", FixedVersion: "1.1.1d",
		URL: "https://avd.aquasec.com/nvd/cve-2024-1234", ServerName: "机器A",
	}
	p := buildFindingExplainPrompt(in, "")
	for _, want := range []string{
		"CVE-2024-1234", "openssl", "1.1.1c", "1.1.1d", "机器A",
		"运行中容器镜像 app:2.0", // 内部 registry 地址剥离,只留末段
		"只输出解读内容本身",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt 缺 %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "registry.internal") {
		t.Fatalf("内部 registry 地址不应出网:\n%s", p)
	}
	if strings.Contains(p, "参考资料") {
		t.Fatal("无搜索上下文时不应出现参考资料段")
	}

	// 搜索资料段:注入 + 提示以扫描字段为准。
	p = buildFindingExplainPrompt(in, `[{"title":"CVE-2024-1234 advisory","url":"https://nvd...","snippet":"RCE in openssl"}]`)
	if !strings.Contains(p, "实时检索参考资料") || !strings.Contains(p, "advisory") ||
		!strings.Contains(p, "以上述扫描字段为准") {
		t.Fatalf("搜索资料段不符:\n%s", p)
	}

	// 超长标题截断。
	in.Title = strings.Repeat("长", 400)
	if p = buildFindingExplainPrompt(in, ""); strings.Count(p, "长") > 220 {
		t.Fatalf("超长标题应截断:%d", strings.Count(p, "长"))
	}
}
