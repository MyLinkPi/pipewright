package build

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/huangchengsir/pipewright/internal/ai"
	"github.com/huangchengsir/pipewright/internal/dagrun"
	"github.com/huangchengsir/pipewright/internal/opschat"
	"github.com/huangchengsir/pipewright/internal/pipeline"
	"github.com/huangchengsir/pipewright/internal/project"
	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// ai_node.go 实现「AI 测试节点(ai_test)」执行器(specs/ai-test-node.md)。
//
// 控制面执行:发现仓库约定目录(.pipewright/tests/requirements/**.md)的需求 →
// ai.Service.AgentTest 逐条跑 agent 会话(HTTP 动作集 P1;浏览器动作集 P2 经容器桥)→
// 裁决一律 advisory(节点不因裁决失败,只有基础设施错误才失败)→ 同构落库测试报告。
//
// AI 未注入 → 「未注入,跳过」(同 ecs_power/oss_upload 向后兼容);AI 未配置 → 按
// onAiUnavailable 裁决(缺省 fail——测试静默跳过比失败更危险)。

// stageAITester 抽象「执行 AI 测试」的能力(单方法;main 注入 ai.Service)。
type stageAITester interface {
	AgentTest(ctx context.Context, in ai.AITestInput) (*ai.AITestResult, error)
}

// aiTestInfra 抽象部署目标解析与被测机执行(main 注入 targetSvc 适配;nil → deployJob
// 绑定退化为日志说明,不阻断)。
type aiTestInfra interface {
	// AITestDeployTarget 解析部署节点的目标机(serverId 钉死形式返回 host;标签选择器
	// 无唯一目标机时返回错误由调用方降级)。
	AITestDeployTarget(ctx context.Context, deployJob pipeline.Job) (serverID, host string, err error)
	// AITestRemoteExec 在指定服务器上以 argv 数组执行命令(无 shell),返回合并输出。
	AITestRemoteExec(ctx context.Context, serverID string, argv []string) (string, error)
}

// aiTestContainerCLI 抽象浏览器桥容器编排所需的容器 CLI 流式执行(参数数组,绝不拼
// shell;main 注入与本地 driver 同源的 commander;nil → 浏览器动作集不可用,退化为
// 纯 HTTP + 日志说明)。
type aiTestContainerCLI interface {
	AITestStream(ctx context.Context, args []string, onLine func(stream, line string)) (int, error)
}

const (
	aiTestRequirementsRoot = ".pipewright/tests/requirements"
	aiTestBrowserImage     = "mcr.microsoft.com/playwright:v1.49.0-noble"
	aiTestBridgePort       = 3111
)

// isAITestJob 判断是否「AI 测试」节点(画布 ai_test 类型)。
func isAITestJob(jobType string) bool {
	return strings.TrimSpace(jobType) == pipeline.AITestType
}

// aiTestShellCLI 用与本地 driver 同源的 execCommander 驱动容器 CLI(浏览器桥编排)。
type aiTestShellCLI struct {
	bin  string
	cmdr Commander
}

// DetectAITestContainerCLI 探测控制机容器 CLI(docker/nerdctl/podman)并返回浏览器桥
// 编排句柄;探测失败(瘦控制机)返回错误 → main 不注入,浏览器动作集降级为纯 HTTP。
func DetectAITestContainerCLI() (aiTestContainerCLI, error) {
	drv, err := DetectDriver(nil)
	if err != nil {
		return nil, err
	}
	return &aiTestShellCLI{bin: drv.Binary(), cmdr: execCommander{}}, nil
}

func (c *aiTestShellCLI) AITestStream(ctx context.Context, args []string, onLine func(stream, line string)) (int, error) {
	return c.cmdr.Stream(ctx, c.bin, args, "", onLine)
}

// ─── 需求发现(本地/远程同一抽象) ─────────────────────────────────────────────

// aiTestReqSource 抽象「列出 + 读取」仓库需求文件(本地工作区 / 远程构建机 SSH 同语义)。
type aiTestReqSource interface {
	List() ([]string, error) // 相对仓库根,正斜杠;约定目录不存在返回空
	Read(rel string) ([]byte, error)
}

type localAITestSource struct{ root string }

func (s localAITestSource) List() ([]string, error) {
	base := filepath.Join(s.root, filepath.FromSlash(aiTestRequirementsRoot))
	if st, err := os.Stat(base); err != nil || !st.IsDir() {
		return nil, nil
	}
	var out []string
	_ = filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(p, ".md") {
			return nil
		}
		if rel, rerr := filepath.Rel(s.root, p); rerr == nil {
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out, nil
}

func (s localAITestSource) Read(rel string) ([]byte, error) {
	p := filepath.Join(s.root, filepath.FromSlash(rel))
	if !strings.HasPrefix(filepath.Clean(p), filepath.Clean(s.root)+string(os.PathSeparator)) {
		return nil, fmt.Errorf("路径越界:%s", rel)
	}
	return os.ReadFile(p)
}

type remoteAITestSource struct {
	listFn func() ([]string, error)
	readFn func(rel string) ([]byte, error)
}

func (s remoteAITestSource) List() ([]string, error) { return s.listFn() }
func (s remoteAITestSource) Read(rel string) ([]byte, error) {
	return s.readFn(rel)
}

// aiTestGlobMatcher 把 filter glob(** 跨段、* 单段内、? 单字符)编译为相对
// requirements 根的匹配器;filter 给全路径(.pipewright/tests/requirements/ 前缀)时剥前缀。
func aiTestGlobMatcher(filter string) (*regexp.Regexp, error) {
	f := strings.TrimSpace(filter)
	f = strings.ReplaceAll(f, "\\", "/")
	f = strings.TrimPrefix(f, aiTestRequirementsRoot+"/")
	if f == "" {
		f = "**/*.md"
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(f); i++ {
		switch f[i] {
		case '*':
			if i+1 < len(f) && f[i+1] == '*' {
				i++
				for i+1 < len(f) && f[i+1] == '/' {
					i++
				}
				b.WriteString(".*")
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '.':
			b.WriteString("\\.")
		default:
			b.WriteString(regexp.QuoteMeta(string(f[i])))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// ─── 执行器 ───────────────────────────────────────────────────────────────────

// runAITestJob 执行一个 ai_test 节点。src 提供需求文件(本地/远程);reportSink 落库
// 同构测试报告(best-effort)。裁决不改变节点成败;基础设施错误(发现 0 需求/解析失败/
// AI 不可达且 fail)→ ErrBuildFailed。
func (b *Builder) runAITestJob(ctx context.Context, rep dagrun.StageReporter, jb pipeline.Job, r *run.Run, stage pipeline.Stage, src aiTestReqSource, reportSink TestReportSink) error {
	if b.aiTester == nil {
		_ = rep.Log(ctx, streamStdout, "· AI 测试节点:AI 服务未注入,跳过")
		return nil
	}
	log := func(msg string) { _ = rep.Log(ctx, streamStdout, msg) }
	fail := func(format string, args ...any) error {
		msg := fmt.Sprintf(format, args...)
		_ = rep.Log(ctx, streamError, "AI 测试节点:"+msg)
		return ErrBuildFailed
	}

	// ── 配置解析(枚举合法性由保存期 validateAITest 保证;此处宽容缺省)。
	cfgS := func(k string) string { return cfgString(jb.Config, k) }
	filter := strings.TrimSpace(cfgS("filter"))
	if filter == "" {
		filter = aiTestRequirementsRoot + "/**/*.md"
	}
	matcher, merr := aiTestGlobMatcher(filter)
	if merr != nil {
		return fail("filter 非法:%v", merr)
	}
	onUnavailable := cfgS("onAiUnavailable")
	timeoutSec := cfgNonNegInt(jb.Config, "timeoutSeconds")
	if timeoutSec == 0 {
		timeoutSec = 300
	}
	maxSteps := cfgNonNegInt(jb.Config, "maxSteps")
	if maxSteps == 0 {
		maxSteps = 25
	}
	params := map[string]string{}
	for k, v := range r.Trigger.Params {
		params[k] = v
	}
	nodeTarget := aiTestExpandVars(cfgS("target"), params)

	// ── 发现需求(fail fast:目录不存在/0 需求/解析失败/id 重复都诚实失败)。
	files, lerr := src.List()
	if lerr != nil {
		return fail("需求目录读取失败:%v", lerr)
	}
	content := map[string][]byte{}
	for _, rel := range files {
		if !matcher.MatchString(strings.TrimPrefix(strings.TrimPrefix(rel, aiTestRequirementsRoot), "/")) {
			continue
		}
		data, rerr := src.Read(rel)
		if rerr != nil {
			return fail("需求文件读取失败 %s:%v", rel, rerr)
		}
		content[rel] = data
	}
	if len(content) == 0 {
		return fail("发现 0 个需求(约定路径 %s,glob %s)——路径打错不该静默绿灯,按失败处理", aiTestRequirementsRoot, filter)
	}
	reqs, perr := ai.ParseAITestRequirements(content)
	if perr != nil {
		return fail("%v", perr)
	}
	// 目标解析:需求 target(${VAR} 已含在正文配置里由 frontmatter 声明) > 节点 target;
	// ${VAR} 展开在此进行(运行参数 + DEPLOY_HOST)。
	deployHost := ""
	if ref := strings.TrimSpace(cfgS("deployJob")); ref != "" {
		if b.aiInfra == nil {
			log(fmt.Sprintf("· deployJob %q:目标解析服务未注入,跳过绑定", ref))
		} else if dj, ok := aiTestFindDeployJob(stage, ref); !ok {
			return fail("deployJob %q 在运行 spec 中不存在", ref)
		} else if _, host, derr := b.aiInfra.AITestDeployTarget(ctx, dj); derr != nil {
			log(fmt.Sprintf("· deployJob %q 目标解析失败(%v),不绑定 DEPLOY_HOST", ref, derr))
		} else {
			deployHost = host
			log(fmt.Sprintf("· deployJob %q → 被测机 %s(内网抓取经该机执行)", ref, deployHost))
		}
	}
	if deployHost != "" {
		params["DEPLOY_HOST"] = deployHost
		nodeTarget = aiTestExpandVars(nodeTarget, params)
	}
	blocker, suggest := 0, 0
	for i := range reqs {
		reqs[i].Target = aiTestExpandVars(reqs[i].Target, params)
		if reqs[i].Target == "" {
			reqs[i].Target = nodeTarget
		}
		if reqs[i].Severity == ai.AITestSeveritySuggest {
			suggest++
		} else {
			blocker++
		}
	}
	for _, rq := range reqs {
		if strings.TrimSpace(rq.Target) == "" {
			return fail("需求 %s 无被测对象(需求与节点的 target 两级皆空)", rq.Path)
		}
	}
	ordered, oerr := ai.OrderAITestRequirements(reqs)
	if oerr != nil {
		return fail("%v", oerr)
	}
	names := make([]string, 0, len(ordered))
	for _, rq := range ordered {
		names = append(names, rq.ID)
	}
	log(fmt.Sprintf("发现 %d 个需求(blocker %d / suggest %d)+ 执行序:%s", len(ordered), blocker, suggest, strings.Join(names, " → ")))

	// ── 动作执行器:HTTP 集(恒有);浏览器集(桥接可用且节点未被禁用时叠加)。
	httpExec := &ai.HTTPActions{Client: b.safeHTTPClient()}
	if deployHost != "" && b.aiInfra != nil {
		httpExec.Vantage = func(vctx context.Context, method, rawURL string, headers map[string]string, body string) (string, error) {
			return aiTestVantageCurl(vctx, b.aiInfra, deployHost, method, rawURL, headers, body)
		}
	}
	executor := ai.AITestExecutor(httpExec)

	var bridge *aiTestBrowserBridge
	if img := strings.TrimSpace(cfgS("browserImage")); b.aiContainer == nil {
		log("· 浏览器动作集未启用(容器 CLI 未注入);仅 HTTP 验收")
	} else {
		if img == "" {
			img = aiTestBrowserImage
		}
		var berr error
		bridge, berr = startAITestBrowserBridge(ctx, b.aiContainer, img, log)
		if berr != nil {
			_ = rep.Log(ctx, streamError, "浏览器桥启动失败(本需求批次仅 HTTP 验收):"+berr.Error())
		} else {
			defer bridge.stop(log)
			browserExec := &aiTestBrowserActions{bridge: bridge, rep: rep, runID: r.ID}
			executor = aiTestCombinedExecutor{http: httpExec, browser: browserExec}
		}
	}

	// ── 会话执行(超时包裹;裁决一律 advisory)。
	actx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()
	result, aerr := b.aiTester.AgentTest(actx, ai.AITestInput{
		Requirements:       ordered,
		DefaultTarget:      nodeTarget,
		MaxSteps:           maxSteps,
		Model:              cfgS("model"),
		ModelContextTokens: cfgNonNegInt(jb.Config, "modelContextTokens"),
		Executor:           executor,
		Log:                log,
	})
	if aerr != nil {
		if actx.Err() != nil {
			return fail("整体超时(%ds)中断:%v", timeoutSec, aerr)
		}
		if strings.Contains(aerr.Error(), "not configured") || aerr == ai.ErrAINotConfigured {
			if onUnavailable == "skip" {
				log("· AI 未配置/不可达(onAiUnavailable=skip):记「未测试」放行")
				return nil
			}
			return fail("AI 未配置/不可达(onAiUnavailable=fail):%v", aerr)
		}
		return fail("%v", aerr)
	}

	// ── 结果呈现:逐条裁决 + 汇总(用量/费用)+ 同构落库(永远不因裁决失败)。
	for _, s := range result.Sessions {
		switch {
		case s.Skipped:
			log(fmt.Sprintf("⏭ %s:跳过 — %s", s.Requirement.ID, s.SkipReason))
		case s.Verdict != nil && s.Verdict.Pass:
			log(fmt.Sprintf("✅ %s(%s):%s | 证据:%s | %d 轮", s.Requirement.ID, s.Requirement.Severity, s.Verdict.Reason, s.Verdict.Evidence, s.Rounds))
		default:
			reason, evidence := "未得出裁决", ""
			if s.Verdict != nil {
				reason, evidence = s.Verdict.Reason, s.Verdict.Evidence
			}
			log(fmt.Sprintf("❌ %s(%s):%s | 证据:%s | %d 轮", s.Requirement.ID, s.Requirement.Severity, reason, evidence, s.Rounds))
		}
	}
	amt := ""
	if result.ConsumedAmount > 0 {
		amt = fmt.Sprintf(" · 花费 %.6g(网关计价)", result.ConsumedAmount)
	}
	log(fmt.Sprintf("📊 AI 验收:%d 通过 / %d 失败 / %d 跳过 · %d 轮 · 输入 %dk / 输出 %dk tok%s · 模型 %s · %ds",
		result.Passed, result.Failed, result.Skipped, result.TotalRounds, result.InputTokens/1000, result.OutputTokens/1000, amt, result.Model, result.DurationMs/1000))

	if reportSink != nil {
		tr := run.TestReport{
			RunID:       r.ID,
			StageID:     stage.ID,
			StageName:   stage.Name,
			Format:      "ai-acceptance",
			Total:       len(result.Sessions),
			Passed:      result.Passed,
			Failed:      result.Failed,
			Skipped:     result.Skipped,
			DurationSec: float64(result.DurationMs) / 1000,
			GateEnabled: false, // 裁决一律 advisory:放行决策归人工审批门
			GatePassed:  true,
			GateReason:  fmt.Sprintf("AI 验收(模型 %s):%d 轮 · 输入 %d / 输出 %d tok · 费用 %.6g", result.Model, result.TotalRounds, result.InputTokens, result.OutputTokens, result.ConsumedAmount),
		}
		if _, serr := reportSink.SaveTestReport(actx, tr); serr != nil {
			log("测试报告落库失败(不影响裁决呈现)")
		}
	}
	return nil
}

// runAITestJobWithClone 是分发点的统一入口:按 run 的 revision 克隆到临时目录读取需求
// (三路径同语义;本地裸镜像缓存下克隆为秒级增量),结束清理。被推送源(pushed)项目
// 没有可克隆的 git 仓库 → 诚实失败(需求文件无处存放)。
func (b *Builder) runAITestJobWithClone(ctx context.Context, rep dagrun.StageReporter, jb pipeline.Job, r *run.Run, stage pipeline.Stage, reportSink TestReportSink) error {
	if b.aiTester == nil {
		_ = rep.Log(ctx, streamStdout, "· AI 测试节点:AI 服务未注入,跳过")
		return nil
	}
	proj, _, perr := b.resolve(ctx, r)
	if perr != nil || proj == nil {
		_ = rep.Log(ctx, streamError, "AI 测试节点:无法加载项目配置")
		return ErrBuildFailed
	}
	if proj.SourceType == project.SourceTypePushed {
		_ = rep.Log(ctx, streamError, "AI 测试节点:被推送源项目无 git 仓库,需求文件无处存放(约定目录依赖 git)")
		return ErrBuildFailed
	}
	dir, derr := os.MkdirTemp("", "pw-aitest-reqs-*")
	if derr != nil {
		_ = rep.Log(ctx, streamError, "AI 测试节点:临时目录创建失败:"+derr.Error())
		return ErrBuildFailed
	}
	defer func() { _ = os.RemoveAll(dir) }()
	auth := b.revealGitAuth(proj.CredentialID)
	_, cerr := b.cloner.Clone(ctx, proj.RepoURL, auth, r.Trigger.Branch, r.Trigger.Commit, dir)
	auth = vault.GitAuth{}
	if cerr != nil {
		if ctx.Err() != nil {
			return run.ErrCanceled
		}
		_ = rep.Log(ctx, streamError, "AI 测试节点:需求源克隆失败(鉴权/网络/ref 不存在)")
		return ErrBuildFailed
	}
	return b.runAITestJob(ctx, rep, jb, r, stage, localAITestSource{root: dir}, reportSink)
}

// aiTestExpandVars 展开 ${KEY} 占位(spec 约定语法;平台的 renderTemplate 是 {{param}}
// 语法、服务于部署节点键,这里独立实现不混用)。
func aiTestExpandVars(s string, vars map[string]string) string {
	if s == "" || !strings.Contains(s, "${") {
		return s
	}
	out := s
	for k, v := range vars {
		out = strings.ReplaceAll(out, "${"+k+"}", v)
	}
	return out
}

// aiTestFindDeployJob 在同阶段 job 里按名找部署节点(deployJob 引用;保存期已校验同阶段存在)。
func aiTestFindDeployJob(stage pipeline.Stage, name string) (pipeline.Job, bool) {
	for _, j := range stage.Jobs {
		if j.Name == name {
			return j, true
		}
	}
	return pipeline.Job{}, false
}

// aiTestVantageCurl 在被测机上用 curl 执行 HTTP 观察(argv 数组,无 shell;-i 含状态行)。
func aiTestVantageCurl(ctx context.Context, infra aiTestInfra, serverID, method, rawURL string, headers map[string]string, body string) (string, error) {
	args := []string{"curl", "-sS", "-i", "--max-time", "25", "-X", method}
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "-H", k+": "+headers[k])
	}
	if body != "" {
		args = append(args, "--data-binary", body)
	}
	args = append(args, rawURL)
	out, err := infra.AITestRemoteExec(ctx, serverID, args)
	if err != nil {
		return "", err
	}
	if len(out) > 16<<10 {
		out = out[:16<<10] + "\n…(已截断)"
	}
	return out, nil
}

func (b *Builder) safeHTTPClient() *http.Client {
	if b.aiHTTP != nil {
		return b.aiHTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// ─── 浏览器动作集(P2):容器桥 + browser_* 工具 ─────────────────────────────

// aiTestCombinedExecutor 把 HTTP 与浏览器动作集合并呈现给 agent(按工具 ID 前缀分派)。
type aiTestCombinedExecutor struct {
	http    *ai.HTTPActions
	browser *aiTestBrowserActions
}

func (c aiTestCombinedExecutor) AITestTools() []opschat.Tool {
	out := append([]opschat.Tool{}, c.http.AITestTools()...)
	return append(out, c.browser.AITestTools()...)
}

func (c aiTestCombinedExecutor) AITestExecute(ctx context.Context, toolID string, args json.RawMessage, log func(string)) (string, error) {
	if strings.HasPrefix(toolID, "browser_") {
		return c.browser.AITestExecute(ctx, toolID, args, log)
	}
	return c.http.AITestExecute(ctx, toolID, args, log)
}

// aiTestBrowserActions 经容器桥执行 browser_* 动作;截图自动登记为 run 产物(留档)。
type aiTestBrowserActions struct {
	bridge *aiTestBrowserBridge
	rep    dagrun.StageReporter
	runID  string
}

func (a *aiTestBrowserActions) AITestTools() []opschat.Tool {
	urlProp := `{"type":"string","maxLength":2048}`
	textProp := `{"type":"string","maxLength":256}`
	return []opschat.Tool{
		{ID: "browser_goto", Mutation: false, Schema: json.RawMessage(`{"type":"object","properties":{"url":` + urlProp + `},"required":["url"],"additionalProperties":false}`)},
		{ID: "browser_read", Mutation: false, Schema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)},
		{ID: "browser_screenshot", Mutation: false, Schema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)},
		{ID: "browser_hover", Mutation: false, Schema: json.RawMessage(`{"type":"object","properties":{"text":` + textProp + `},"required":["text"],"additionalProperties":false}`)},
		{ID: "browser_click", Mutation: true, Schema: json.RawMessage(`{"type":"object","properties":{"text":` + textProp + `},"required":["text"],"additionalProperties":false}`)},
		{ID: "browser_fill", Mutation: true, Schema: json.RawMessage(`{"type":"object","properties":{"label":` + textProp + `,"value":{"type":"string","maxLength":1024}},"required":["label","value"],"additionalProperties":false}`)},
		{ID: "browser_select", Mutation: true, Schema: json.RawMessage(`{"type":"object","properties":{"label":` + textProp + `,"value":{"type":"string","maxLength":1024}},"required":["label","value"],"additionalProperties":false}`)},
	}
}

func (a *aiTestBrowserActions) AITestExecute(ctx context.Context, toolID string, args json.RawMessage, log func(string)) (string, error) {
	var in struct {
		URL   string `json:"url"`
		Text  string `json:"text"`
		Label string `json:"label"`
		Value string `json:"value"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("参数非法:%w", err)
		}
	}
	action := strings.TrimPrefix(toolID, "browser_")
	payload := map[string]any{"action": action, "url": in.URL, "text": in.Text, "label": in.Label, "value": in.Value}
	out, err := a.bridge.act(ctx, action, payload)
	if err != nil {
		return "", err
	}
	// 截图留档:桥返回 "screenshot saved: <file>" → 登记为 run 产物(真实字节由产物库归档)。
	if action == "screenshot" {
		if i := strings.LastIndex(out, ": "); i >= 0 {
			file := strings.TrimSpace(out[i+2:])
			p := filepath.Join(a.bridge.dir, "shots", filepath.Base(file))
			if st, serr := os.Stat(p); serr == nil {
				_ = a.rep.EmitArtifact(ctx, run.Artifact{
					RunID:     a.runID,
					Type:      "file",
					Name:      "ai-test-" + filepath.Base(file),
					Reference: p,
					SizeBytes: st.Size(),
					Metadata:  map[string]any{"kind": "ai-test-screenshot"},
				})
			}
		}
	}
	return out, nil
}

// aiTestBrowserBridge 是 Playwright 动作桥:临时目录放 action-server.js,docker run -d
// (--init --shm-size) 起 mcr playwright 镜像,控制面经 127.0.0.1:<port> 驱动。
type aiTestBrowserBridge struct {
	container string
	baseURL   string
	client    *http.Client
	dir       string
	cli       aiTestContainerCLI
}

// startAITestBrowserBridge 拉起动作桥并等待健康;失败返回错误(调用方降级为纯 HTTP)。
func startAITestBrowserBridge(ctx context.Context, cli aiTestContainerCLI, image string, log func(string)) (*aiTestBrowserBridge, error) {
	dir, err := os.MkdirTemp("", "pw-aitest-bridge-*")
	if err != nil {
		return nil, err
	}
	if werr := os.WriteFile(filepath.Join(dir, "action-server.js"), []byte(aiTestActionServerJS), 0o644); werr != nil {
		_ = os.RemoveAll(dir)
		return nil, werr
	}
	shots := filepath.Join(dir, "shots")
	if mkErr := os.MkdirAll(shots, 0o755); mkErr != nil {
		_ = os.RemoveAll(dir)
		return nil, mkErr
	}
	container := "pw-aitest-" + fmt.Sprintf("%d", time.Now().UnixNano()%1e9)
	// host 端口由 docker 原子分配(127.0.0.1::3111 = 只绑回环、端口留空随机):
	// 消除「探测后、绑定前」的 TOCTOU 窗口与固定端口池上限;实际端口经 docker port 查回。
	args := []string{
		"run", "-d", "--name", container,
		"--init", "--shm-size=1g",
		"-p", fmt.Sprintf("127.0.0.1::%d", aiTestBridgePort),
		"-v", dir + ":/srv:rw",
		image, "node", "/srv/action-server.js",
	}
	if code, rerr := cli.AITestStream(ctx, args, func(_, line string) { log("  " + line) }); rerr != nil || code != 0 {
		_, _ = cli.AITestStream(ctx, []string{"rm", "-f", container}, func(_ string, _ string) {})
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("容器启动失败(exit=%d):%v", code, rerr)
	}
	hostPort, qerr := aiTestContainerHostPort(ctx, cli, container)
	if qerr != nil {
		_, _ = cli.AITestStream(context.Background(), []string{"rm", "-f", container}, func(_ string, _ string) {})
		_ = os.RemoveAll(dir)
		return nil, qerr
	}
	b := &aiTestBrowserBridge{
		container: container,
		baseURL:   fmt.Sprintf("http://127.0.0.1:%d", hostPort),
		client:    &http.Client{Timeout: 60 * time.Second},
		dir:       dir,
		cli:       cli,
	}
	// 健康等待(浏览器冷启动 + 镜像首拉可能较慢;≤120s)。
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			b.stop(log)
			return nil, ctx.Err()
		}
		resp, herr := b.client.Get(b.baseURL + "/healthz")
		if herr == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				log("· 浏览器动作桥就绪(image " + image + ")")
				return b, nil
			}
		}
		time.Sleep(2 * time.Second)
	}
	b.stop(log)
	return nil, fmt.Errorf("动作桥健康检查超时")
}

func (b *aiTestBrowserBridge) stop(log func(string)) {
	if b.cli != nil {
		_, _ = b.cli.AITestStream(context.Background(), []string{"rm", "-f", b.container}, func(_ string, _ string) {})
	}
	if b.dir != "" {
		_ = os.RemoveAll(b.dir)
	}
	if log != nil {
		log("· 浏览器动作桥已停止")
	}
}

// act 执行一个动作并返回观察文本。
func (b *aiTestBrowserBridge) act(ctx context.Context, action string, payload map[string]any) (string, error) {
	raw, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.baseURL+"/act", bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		OK          bool   `json:"ok"`
		Observation string `json:"observation"`
		Error       string `json:"error"`
	}
	if jerr := json.Unmarshal(data, &out); jerr != nil {
		return "", fmt.Errorf("桥响应不可解析(%d)", resp.StatusCode)
	}
	if !out.OK {
		return "", fmt.Errorf("%s", out.Error)
	}
	return out.Observation, nil
}

// aiTestContainerHostPort 查回 docker 为容器 3111 端口实际分配的本机端口
// (`docker port <c> 3111` 输出形如 127.0.0.1:34567,取行尾数字;短暂重试容忍启动竞态)。
func aiTestContainerHostPort(ctx context.Context, cli aiTestContainerCLI, container string) (int, error) {
	last := ""
	for attempt := 0; attempt < 4; attempt++ {
		var out strings.Builder
		code, err := cli.AITestStream(ctx, []string{"port", container, strconv.Itoa(aiTestBridgePort)}, func(_ string, line string) {
			out.WriteString(line)
			out.WriteString("\n")
		})
		if err == nil && code == 0 {
			for _, line := range strings.Split(out.String(), "\n") {
				line = strings.TrimSpace(line)
				if line != "" {
					last = line
				}
				if i := strings.LastIndex(line, ":"); i >= 0 {
					if p, perr := strconv.Atoi(line[i+1:]); perr == nil && p > 0 {
						return p, nil
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return 0, fmt.Errorf("无法查询动作桥端口(docker port 输出:%q)", last)
}

// aiTestActionServerJS 是容器内动作服务(Playwright 官方镜像自带 node + playwright):
// POST /act {action,url,text,label,value,file} → {ok,observation|error};GET /healthz。
// 浏览器以 --no-sandbox --disable-dev-shm-usage 启动(容器内标准姿势);页面文本摘要
// (标题/标题层级/按钮/链接/输入框/关键文本)作为 read 的观察;截图存 /srv/shots。
const aiTestActionServerJS = `
const http = require('http');
const { chromium } = require('playwright');
const fs = require('fs');

let browser = null, page = null, seq = 0;

async function ensure() {
  if (page) return page;
  browser = await chromium.launch({ args: ['--no-sandbox', '--disable-dev-shm-usage'] });
  page = await browser.newPage({ viewport: { width: 1280, height: 800 } });
  return page;
}

async function outline(p) {
  return await p.evaluate(() => {
    const lines = [];
    const push = (t) => { if (t && lines.length < 220) lines.push(t.trim().slice(0, 160)); };
    push('TITLE: ' + (document.title || ''));
    push('URL: ' + location.href);
    document.querySelectorAll('h1,h2,h3,h4').forEach((e) => push('H: ' + e.innerText));
    document.querySelectorAll('button,input[type=submit],a').forEach((e) => {
      const t = (e.innerText || e.value || '').trim();
      if (t) push((e.tagName === 'A' ? 'LINK' : 'BTN') + ': ' + t);
    });
    document.querySelectorAll('input,select,textarea').forEach((e) => {
      const label = (e.labels && e.labels[0] && e.labels[0].innerText) || e.placeholder || e.name || '';
      if (label) push('FIELD: ' + label.trim());
    });
    return lines.join('\n').slice(0, 12000);
  });
}

async function act(a) {
  const p = await ensure();
  switch (a.action) {
    case 'goto':
      await p.goto(a.url, { waitUntil: 'domcontentloaded', timeout: 45000 });
      await p.waitForTimeout(600);
      return 'navigated to ' + a.url + '\n' + (await outline(p));
    case 'read':
      return await outline(p);
    case 'click': {
      const t = p.getByText(a.text, { exact: false }).first();
      if (await t.count() > 0) { await t.click({ timeout: 15000 }); }
      else { await p.getByRole('button', { name: a.text }).first().click({ timeout: 15000 }); }
      await p.waitForTimeout(800);
      return 'clicked ' + JSON.stringify(a.text) + '\n' + (await outline(p));
    }
    case 'fill': {
      const label = a.label || a.text;
      let f = p.getByLabel(label, { exact: false }).first();
      if (await f.count() === 0) f = p.getByPlaceholder(label).first();
      if (await f.count() === 0) f = p.locator('[name=' + JSON.stringify(label) + ']').first();
      await f.fill(String(a.value ?? ''), { timeout: 15000 });
      return 'filled ' + JSON.stringify(label);
    }
    case 'select': {
      const f = p.getByLabel(a.label || a.text, { exact: false }).first();
      await f.selectOption(String(a.value ?? ''), { timeout: 15000 });
      return 'selected ' + JSON.stringify(a.value) + ' in ' + JSON.stringify(a.label);
    }
    case 'hover': {
      await p.getByText(a.text, { exact: false }).first().hover({ timeout: 15000 });
      return 'hovered ' + JSON.stringify(a.text);
    }
    case 'screenshot': {
      seq++;
      const file = 'shot-' + String(seq).padStart(3, '0') + '.png';
      await p.screenshot({ path: '/srv/shots/' + file, fullPage: false });
      return 'screenshot saved: ' + file;
    }
    default:
      throw new Error('unknown action ' + a.action);
  }
}

http.createServer(async (req, res) => {
  if (req.method === 'GET' && req.url === '/healthz') {
    res.writeHead(200); res.end('ok'); return;
  }
  if (req.method === 'POST' && req.url === '/act') {
    let body = '';
    req.on('data', (c) => { body += c; if (body.length > 65536) req.destroy(); });
    req.on('end', async () => {
      try {
        const a = JSON.parse(body || '{}');
        const observation = await act(a);
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify({ ok: true, observation }));
      } catch (e) {
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify({ ok: false, error: String(e && e.message || e) }));
      }
    });
    return;
  }
  res.writeHead(404); res.end();
}).listen(3111, '0.0.0.0', () => console.log('ai-test action server on 3111'));
`
