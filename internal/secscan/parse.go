package secscan

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// 本文件是 trivy/nuclei 输出 → 统一 Finding 的纯解析器(无 IO,重点单测)。

// severityRank 是严重度排序权重(升序 = critical 在前)。
func severityRank(sev string) int {
	switch sev {
	case SeverityCritical:
		return 0
	case SeverityHigh:
		return 1
	case SeverityMedium:
		return 2
	case SeverityLow:
		return 3
	case SeverityInfo:
		return 4
	default:
		return 5
	}
}

// normSeverity 把 trivy/nuclei 的严重度归一到小写枚举;无法识别 → unknown。
func normSeverity(sev string) string {
	switch strings.ToLower(strings.TrimSpace(sev)) {
	case "critical":
		return SeverityCritical
	case "high":
		return SeverityHigh
	case "medium", "moderate":
		return SeverityMedium
	case "low":
		return SeverityLow
	case "info", "informational", "none":
		return SeverityInfo
	default:
		return SeverityUnknown
	}
}

// validScanType 报告扫描类型枚举是否合法。
func validScanType(t string) bool { return t == ScanTypeTrivy || t == ScanTypeNuclei }

// Summarize 聚合 finding 列表的各级计数。
func Summarize(findings []Finding) Summary {
	s := Summary{Total: len(findings)}
	for _, f := range findings {
		switch f.Severity {
		case SeverityCritical:
			s.Critical++
		case SeverityHigh:
			s.High++
		case SeverityMedium:
			s.Medium++
		case SeverityLow:
			s.Low++
		case SeverityInfo:
			s.Info++
		default:
			s.Unknown++
		}
	}
	return s
}

// truncateRunes 按字符截断(防超长 CVE 描述撑爆报告)。
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

// ---------- Trivy(JSON) ----------

// trivyReport 是 trivy JSON 输出的最小子集(其余字段忽略)。
type trivyReport struct {
	Results []struct {
		Target          string `json:"Target"`
		Vulnerabilities []struct {
			VulnerabilityID  string `json:"VulnerabilityID"`
			PkgName          string `json:"PkgName"`
			InstalledVersion string `json:"InstalledVersion"`
			FixedVersion     string `json:"FixedVersion"`
			PrimaryURL       string `json:"PrimaryURL"`
			Title            string `json:"Title"`
			Severity         string `json:"Severity"`
		} `json:"Vulnerabilities"`
	} `json:"Results"`
}

// TrivyFindings 解析一次 trivy 扫描的 JSON 输出;source 是这批 finding 的来源标记
//("host" 或 "image:<镜像引用>")。
func TrivyFindings(report []byte, source string) ([]Finding, error) {
	var tr trivyReport
	if err := json.Unmarshal(report, &tr); err != nil {
		return nil, fmt.Errorf("secscan: parse trivy report: %w", err)
	}
	out := make([]Finding, 0)
	for _, res := range tr.Results {
		for _, v := range res.Vulnerabilities {
			title := strings.TrimSpace(v.Title)
			if title == "" {
				title = v.PrimaryURL
			}
			out = append(out, Finding{
				Severity:         normSeverity(v.Severity),
				Source:           source,
				Target:           v.PkgName,
				ID:               v.VulnerabilityID,
				Title:            truncateRunes(title, 256),
				InstalledVersion: v.InstalledVersion,
				FixedVersion:     v.FixedVersion,
				URL:              v.PrimaryURL,
			})
		}
	}
	return out, nil
}

// ---------- Nuclei(JSONL) ----------

// nucleiEvent 是 nuclei -j 单行输出的最小子集。
type nucleiEvent struct {
	TemplateID string `json:"template-id"`
	Info       struct {
		Name      string   `json:"name"`
		Severity  string   `json:"severity"`
		Reference []string `json:"reference"`
	} `json:"info"`
	Host      string `json:"host"`
	MatchedAt string `json:"matched-at"`
}

// NucleiFindings 解析 nuclei 的 JSONL 输出(逐行,坏行静默跳过——nuclei 混排的
// 日志行不应让整份结果报废)。
func NucleiFindings(out []byte) ([]Finding, error) {
	findings := make([]Finding, 0)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] != '{' {
			continue
		}
		var ev nucleiEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.TemplateID == "" && ev.MatchedAt == "" {
			continue
		}
		url := ""
		if len(ev.Info.Reference) > 0 {
			url = ev.Info.Reference[0]
		}
		findings = append(findings, Finding{
			Severity: normSeverity(ev.Info.Severity),
			Source:   "nuclei",
			Target:   ev.MatchedAt,
			ID:       ev.TemplateID,
			Title:    truncateRunes(strings.TrimSpace(ev.Info.Name), 256),
			URL:      url,
		})
	}
	return findings, nil
}

// ---------- 过滤与排序 ----------

// FilterFindings 对全量明细应用抽屉过滤,返回 (总数, 当前页)。severity 精确匹配;
// source 前缀匹配(如 "image" 命中所有 image:<ref>);query 对 ID/Title/Target
// 不区分大小写包含。limit ≤0 → 100,上限 500。
func FilterFindings(fs []Finding, f FindingFilter) (int, []Finding) {
	if f.Limit <= 0 {
		f.Limit = 100
	}
	if f.Limit > 500 {
		f.Limit = 500
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	q := strings.ToLower(f.Query)
	matched := make([]Finding, 0, len(fs))
	for _, x := range fs {
		if f.Severity != "" && x.Severity != f.Severity {
			continue
		}
		if f.Source != "" && !strings.HasPrefix(x.Source, f.Source) {
			continue
		}
		if q != "" &&
			!strings.Contains(strings.ToLower(x.ID+"\n"+x.Title+"\n"+x.Target), q) {
			continue
		}
		matched = append(matched, x)
	}
	end := f.Offset + f.Limit
	if end > len(matched) {
		end = len(matched)
	}
	if f.Offset >= len(matched) {
		return len(matched), []Finding{}
	}
	return len(matched), matched[f.Offset:end]
}

// SortFindings 就地按严重度排序(critical 在前,同级稳定)。
func SortFindings(fs []Finding) {
	// 简单插入排序:finding 量级数千,且报告侧只需稳定有序展示。
	for i := 1; i < len(fs); i++ {
		for j := i; j > 0 && severityRank(fs[j].Severity) < severityRank(fs[j-1].Severity); j-- {
			fs[j], fs[j-1] = fs[j-1], fs[j]
		}
	}
}
