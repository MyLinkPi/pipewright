package httpapi

import (
	"html/template"
	"io"
	"time"

	"github.com/huangchengsir/pipewright/internal/secscan"
)

// HTML 安全审计报告:自包含单文件(内联 CSS,无外链依赖),浏览器「打印 → 另存 PDF」
// 即得正式报告。结构:封面统计(范围/时间/全员严重度合计)→ 每台服务器一节
// (trivy CVE 明细 + nuclei 暴露面明细,严重度降序,分页打印)。

// securityReportModel 是报告模板的数据模型。
type securityReportModel struct {
	GeneratedAt time.Time
	Scope       string // fleet | server
	Servers     []secscan.ServerReport
	Totals      secscan.Summary
	HasAnyData  bool // 任一服务器有 done 结果 → 明细区渲染
}

// severityLabelCN 是严重度的中文标签。
func severityLabelCN(sev string) string {
	switch sev {
	case secscan.SeverityCritical:
		return "严重"
	case secscan.SeverityHigh:
		return "高危"
	case secscan.SeverityMedium:
		return "中危"
	case secscan.SeverityLow:
		return "低危"
	case secscan.SeverityInfo:
		return "信息"
	default:
		return "未知"
	}
}

// severityClass 是严重度徽章的 CSS 类名。
func severityClass(sev string) string {
	switch sev {
	case secscan.SeverityCritical:
		return "sev-critical"
	case secscan.SeverityHigh:
		return "sev-high"
	case secscan.SeverityMedium:
		return "sev-medium"
	case secscan.SeverityLow:
		return "sev-low"
	case secscan.SeverityInfo:
		return "sev-info"
	default:
		return "sev-unknown"
	}
}

// fmtTime 本地无关的统一时间格式(报告用 UTC,避免歧义)。
func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

// findingsCap 报告单节渲染的明细上限(防巨型结果集把打印文件撑爆;徽标计数不受影响)。
const findingsCap = 200

// capFindings 截断明细(防巨型结果集把打印文件撑爆;徽标计数不受影响;模板里
// 另以 len 对比提示截断)。
func capFindings(fs []secscan.Finding) []secscan.Finding {
	if len(fs) <= findingsCap {
		return fs
	}
	return fs[:findingsCap]
}

var securityReportFuncs = template.FuncMap{
	"sevLabel": severityLabelCN,
	"sevClass": severityClass,
	"fmtTime":  fmtTime,
	"cap":      capFindings,
}

// securityReportTmpl 是报告模板(解析一次;模板为静态常量,进程内安全复用)。
var securityReportTmpl = template.Must(template.New("report").Funcs(securityReportFuncs).Parse(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<title>Pipewright 安全审计报告</title>
<style>
  :root { --ink:#1a2233; --muted:#5b6474; --line:#d9dee8; --bg:#f6f7fa; }
  * { box-sizing: border-box; }
  body { font: 14px/1.6 -apple-system, "Segoe UI", "Microsoft YaHei", sans-serif; color: var(--ink); margin: 0; background: var(--bg); }
  .page { max-width: 920px; margin: 0 auto; padding: 32px 24px 64px; }
  h1 { font-size: 26px; margin: 0 0 4px; }
  h2 { font-size: 18px; margin: 28px 0 10px; padding-bottom: 6px; border-bottom: 2px solid var(--ink); }
  h3 { font-size: 15px; margin: 18px 0 8px; }
  .sub { color: var(--muted); font-size: 13px; }
  .cover { background: #fff; border: 1px solid var(--line); border-radius: 10px; padding: 22px 26px; margin-top: 18px; }
  .kpis { display: flex; gap: 10px; flex-wrap: wrap; margin-top: 14px; }
  .kpi { flex: 1 1 100px; border: 1px solid var(--line); border-radius: 8px; padding: 10px 12px; background: #fff; text-align: center; }
  .kpi b { display: block; font-size: 22px; }
  .kpi span { color: var(--muted); font-size: 12px; }
  .server { background: #fff; border: 1px solid var(--line); border-radius: 10px; padding: 18px 22px; margin: 18px 0; }
  table { width: 100%; border-collapse: collapse; font-size: 12.5px; margin-top: 8px; }
  th, td { border: 1px solid var(--line); padding: 6px 8px; text-align: left; vertical-align: top; }
  th { background: #eef1f6; white-space: nowrap; }
  td.num { text-align: right; }
  .sev { display: inline-block; padding: 1px 8px; border-radius: 10px; font-size: 12px; color: #fff; white-space: nowrap; }
  .sev-critical { background: #b3261e; } .sev-high { background: #e8590c; }
  .sev-medium { background: #b08900; } .sev-low { background: #2f6fab; }
  .sev-info { background: #6b7280; } .sev-unknown { background: #9aa1ad; }
  .muted { color: var(--muted); }
  a { color: #2f6fab; text-decoration: none; word-break: break-all; }
  .note { background: #fff8e6; border: 1px solid #ead48a; border-radius: 6px; padding: 8px 10px; font-size: 12.5px; margin-top: 8px; }
  .footer { margin-top: 28px; color: var(--muted); font-size: 12px; text-align: center; }
  @media print {
    body { background: #fff; }
    .page { max-width: none; padding: 0; }
    .server { break-inside: avoid-page; border-radius: 0; }
    .cover, .kpi { border-radius: 0; }
    a { color: inherit; }
  }
</style>
</head>
<body>
<div class="page">
  <h1>Pipewright 安全审计报告</h1>
  <div class="sub">生成时间:{{fmtTime .GeneratedAt}} · 范围:{{if eq .Scope "fleet"}}全部纳管服务器{{else}}单台服务器{{end}} · 数据源:最近一次成功扫描(Trivy CVE / Nuclei 暴露面)</div>

  <div class="cover">
    <div class="sub">覆盖服务器:{{len .Servers}} 台{{if .HasAnyData}} · 汇总下列「最近一次成功结果」的合计{{else}} · 暂无任何已完成扫描{{end}}</div>
    <div class="kpis">
      <div class="kpi"><b>{{.Totals.Critical}}</b><span>严重 Critical</span></div>
      <div class="kpi"><b>{{.Totals.High}}</b><span>高危 High</span></div>
      <div class="kpi"><b>{{.Totals.Medium}}</b><span>中危 Medium</span></div>
      <div class="kpi"><b>{{.Totals.Low}}</b><span>低危 Low</span></div>
      <div class="kpi"><b>{{.Totals.Info}}</b><span>信息 Info</span></div>
      <div class="kpi"><b>{{.Totals.Unknown}}</b><span>未知 Unknown</span></div>
    </div>
  </div>

  {{if not .HasAnyData}}
  <h2>暂无数据</h2>
  <p class="muted">尚未有任何服务器完成过扫描。请到「安全中心」发起全量扫描后再导出报告。</p>
  {{end}}

  {{range .Servers}}
  <section class="server">
    <h2>{{.ServerName}} <span class="muted">({{.Host}})</span></h2>
    {{if not .Trivy}}{{if not .Nuclei}}
    <p class="muted">该服务器尚未完成任何扫描。</p>
    {{end}}{{end}}
    {{if .Trivy}}
    <h3>漏洞扫描(Trivy)· {{fmtTime .Trivy.FinishedAt}}</h3>
    <div class="sub">严重 {{.Trivy.Summary.Critical}} · 高危 {{.Trivy.Summary.High}} · 中危 {{.Trivy.Summary.Medium}} · 低危 {{.Trivy.Summary.Low}} · 未知 {{.Trivy.Summary.Unknown}}{{if .Trivy.Summary.ImageCount}} · 覆盖容器镜像 {{.Trivy.Summary.ImageCount}} 个{{end}}</div>
    {{if .Trivy.Doc}}
      {{if .Trivy.Doc.Notes}}<div class="note">{{range .Trivy.Doc.Notes}}<div>{{.}}</div>{{end}}</div>{{end}}
      {{$fs := cap .Trivy.Doc.Findings}}
      <table>
        <thead><tr><th>严重度</th><th>编号</th><th>软件包 / 目标</th><th>当前版本</th><th>修复版本</th><th>说明</th></tr></thead>
        <tbody>
        {{range $fs}}<tr>
          <td><span class="sev {{sevClass .Severity}}">{{sevLabel .Severity}}</span></td>
          <td>{{if .URL}}<a href="{{.URL}}">{{.ID}}</a>{{else}}{{.ID}}{{end}}</td>
          <td>{{.Target}}</td>
          <td>{{.InstalledVersion}}</td>
          <td>{{.FixedVersion}}</td>
          <td>{{.Title}}</td>
        </tr>{{end}}
        </tbody>
      </table>
      {{if gt (len .Trivy.Doc.Findings) 200}}<div class="note">明细超过 200 条,报告仅展示前 200 条;完整明细请在「安全中心」查看。</div>{{end}}
    {{else}}
      <p class="muted">报告明细不可用(存档缺失),仅展示计数。</p>
    {{end}}
    {{end}}
    {{if .Nuclei}}
    <h3>暴露面检测(Nuclei)· {{fmtTime .Nuclei.FinishedAt}}</h3>
    <div class="sub">严重 {{.Nuclei.Summary.Critical}} · 高危 {{.Nuclei.Summary.High}} · 中危 {{.Nuclei.Summary.Medium}} · 低危 {{.Nuclei.Summary.Low}} · 信息 {{.Nuclei.Summary.Info}}</div>
    {{if .Nuclei.Doc}}
      {{if .Nuclei.Doc.Notes}}<div class="note">{{range .Nuclei.Doc.Notes}}<div>{{.}}</div>{{end}}</div>{{end}}
      {{$fs := cap .Nuclei.Doc.Findings}}
      <table>
        <thead><tr><th>严重度</th><th>模板</th><th>命中位置</th><th>说明</th></tr></thead>
        <tbody>
        {{range $fs}}<tr>
          <td><span class="sev {{sevClass .Severity}}">{{sevLabel .Severity}}</span></td>
          <td>{{if .URL}}<a href="{{.URL}}">{{.ID}}</a>{{else}}{{.ID}}{{end}}</td>
          <td>{{.Target}}</td>
          <td>{{.Title}}</td>
        </tr>{{end}}
        </tbody>
      </table>
      {{if gt (len .Nuclei.Doc.Findings) 200}}<div class="note">明细超过 200 条,报告仅展示前 200 条;完整明细请在「安全中心」查看。</div>{{end}}
    {{else}}
      <p class="muted">报告明细不可用(存档缺失),仅展示计数。</p>
    {{end}}
    {{end}}
  </section>
  {{end}}

  <div class="footer">Pipewright 安全中心生成 · 扫描结果代表扫描时点状态,不构成实时安全承诺</div>
</div>
</body>
</html>
`))

// renderSecurityReport 把报告流式渲染到 w。
func renderSecurityReport(w io.Writer, reports []secscan.ServerReport, now time.Time) error {
	m := securityReportModel{GeneratedAt: now, Servers: reports, Scope: "fleet"}
	for _, rep := range reports {
		if rep.Trivy != nil || rep.Nuclei != nil {
			m.HasAnyData = true
		}
		if rep.Trivy != nil {
			m.Totals.Add(rep.Trivy.Summary)
		}
		if rep.Nuclei != nil {
			m.Totals.Add(rep.Nuclei.Summary)
		}
	}
	if len(reports) == 1 {
		m.Scope = "server"
	}
	return securityReportTmpl.Execute(w, m)
}
