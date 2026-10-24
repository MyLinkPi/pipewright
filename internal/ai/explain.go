// explain.go 是「安全漏洞 AI 解读」:把一条扫描发现(Trivy CVE / Nuclei 命中)构 prompt
// 调 LLM chat,产出通俗的中文解读(是什么 / 危害与利用条件 / 修复建议 / 参考)。
//
// 与 Diagnose 的差异:这是**交互式**能力(用户点按钮取解读),未配置/未启用返回
// ErrAINotConfigured 由 handler 映射 503 + 前端引导配置,而非优雅降级为空结果。
//
// 出网纪律:prompt 只含扫描发现的公开语义字段(编号/包/版本/严重度/标题/参考链接)
// 与服务器显示名;**不含**主机地址/IP(对解读无价值,减少内部信息出网)。无 secret 面。
package ai

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// explainFindingMaxTokens 是解读 chat 的 token 预算(300 字内中文 + Markdown 足够)。
const explainFindingMaxTokens = 800

// FindingExplainer 是安全中心的窄依赖接口(仿 RepoAnalyzer:httpapi 不依赖 ai.Service
// 全集,测试注入 fake 零成本)。
type FindingExplainer interface {
	// ExplainFinding 对一条扫描发现生成人读解读(Markdown);未配置/未启用 →
	// ErrAINotConfigured,其余失败 → 人读 error(绝无密钥)。
	ExplainFinding(ctx context.Context, in ExplainFindingInput) (string, error)
}

// ExplainFindingInput 是一条待解读的扫描发现(公开语义字段;不含主机地址)。
type ExplainFindingInput struct {
	ScanType         string // trivy | nuclei
	Source           string // host | image:<ref> | nuclei
	Severity         string
	VulnID           string // CVE 编号 / 模板 ID
	Title            string
	Target           string // 软件包名 / 命中位置
	InstalledVersion string
	FixedVersion     string
	URL              string
	ServerName       string // 显示名(措辞上下文;不含地址)
}

// ExplainFinding 实现 FindingExplainer:构 prompt → chat → 原文返回(Markdown)。
func (s *service) ExplainFinding(ctx context.Context, in ExplainFindingInput) (string, error) {
	cfg, sealed, err := s.load(ctx)
	if err != nil {
		return "", fmt.Errorf("AI 配置读取失败:%w", err)
	}
	if cfg == nil || !cfg.Configured || !cfg.Enabled {
		return "", ErrAINotConfigured
	}
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL(cfg.Provider)
	}
	if baseURL == "" {
		return "", ErrAINotConfigured
	}
	// 密钥进程内取用即弃(非流式短回包;与 Generate 同原语,失败不泄漏细节)。
	if len(sealed) == 0 {
		return "", ErrAINotConfigured
	}
	apiKey, oerr := s.openSecret(sealed)
	if oerr != nil {
		return "", errors.New("AI 密钥不可用(保险库未配置)")
	}
	out, cerr := s.chatWithTokens(ctx, cfg.Provider, baseURL, cfg.Model, apiKey, buildFindingExplainPrompt(in), explainFindingMaxTokens)
	if cerr != nil {
		return "", fmt.Errorf("AI 解读失败:%w", cerr)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", errors.New("AI 返回了空解读,请重试")
	}
	return out, nil
}

// buildFindingExplainPrompt 构中文解读 prompt(字段空白自然省略对应行)。
func buildFindingExplainPrompt(in ExplainFindingInput) string {
	var b strings.Builder
	b.WriteString("你是资深安全工程师,为服务器运维人员解读一条漏洞扫描结果。用简体中文,通俗易懂、具体可执行,不要泛泛而谈;总长度 300 字以内;用 Markdown 按四个小节组织:\n")
	b.WriteString("**是什么**:漏洞/发现的通俗解释(影响什么组件、成因类型)。\n")
	b.WriteString("**危害与利用条件**:攻击者能做什么、需要什么前提(是否需认证/可触达/特定配置),结合严重度评估实际风险。\n")
	b.WriteString("**修复建议**:结合当前版本与修复版本给具体做法(升级命令给通用示例并注明按发行版调整;无修复版本时给缓解措施)。\n")
	b.WriteString("**参考**:基于编号给出官方参考指引;不确定具体链接时写「参见扫描详情中的链接」。\n\n")
	b.WriteString("扫描发现:\n")
	kind := "Trivy 漏洞扫描(CVE)"
	if in.ScanType == "nuclei" {
		kind = "Nuclei 暴露面/已知漏洞探测"
	}
	fmt.Fprintf(&b, "- 来源:%s,服务器:%s\n", kind, in.ServerName)
	if in.Source != "" {
		fmt.Fprintf(&b, "- 对象:%s\n", findingSourceLabel(in.Source))
	}
	if in.VulnID != "" {
		fmt.Fprintf(&b, "- 编号:%s\n", in.VulnID)
	}
	if in.Severity != "" {
		fmt.Fprintf(&b, "- 严重度:%s\n", in.Severity)
	}
	if in.Target != "" {
		fmt.Fprintf(&b, "- 目标(软件包/命中位置):%s\n", in.Target)
	}
	if in.InstalledVersion != "" {
		fmt.Fprintf(&b, "- 当前版本:%s\n", in.InstalledVersion)
	}
	if in.FixedVersion != "" {
		fmt.Fprintf(&b, "- 已知修复版本:%s\n", in.FixedVersion)
	}
	if in.Title != "" {
		fmt.Fprintf(&b, "- 标题:%s\n", truncateForFindingPrompt(in.Title, 200))
	}
	if in.URL != "" {
		fmt.Fprintf(&b, "- 参考链接:%s\n", truncateForFindingPrompt(in.URL, 200))
	}
	b.WriteString("\n只输出解读内容本身,不要额外寒暄。")
	return b.String()
}

// findingSourceLabel 把来源标记转人话(镜像引用只保留最后一段,减少内部 registry 地址出网)。
func findingSourceLabel(source string) string {
	if source == "host" {
		return "主机已安装软件包"
	}
	if ref, ok := strings.CutPrefix(source, "image:"); ok {
		if i := strings.LastIndex(ref, "/"); i >= 0 {
			ref = ref[i+1:]
		}
		return "运行中容器镜像 " + ref
	}
	return source
}

// truncateForFindingPrompt 截断超长字段(防异常长标题吃 token)。
func truncateForFindingPrompt(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
