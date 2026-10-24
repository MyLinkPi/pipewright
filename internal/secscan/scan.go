package secscan

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/huangchengsir/pipewright/internal/target"
)

// 扫描执行:经 SSH 在目标机上 docker run --rm 跑 trivy(rootfs + 镜像)/nuclei。
//
// 命令纪律(AC-SEC-02):全部 argv 数组化,不拼 shell;镜像引用/目标 URL 是受控
// 常量或经白名单校验的入参。扫描不设主动超时——时长取决于镜像/模板下载,由服务
// 关停上下文(baseCtx)兜底取消;run 终态落库用独立短超时上下文。

// ServerInfo 是执行器视角的目标服务器(target.Server 的最小投影)。
type ServerInfo struct {
	ID   string
	Name string
	Host string
	IsPVE bool
}

// Runner 是扫描执行的抽象(测试注入 fake;生产由 SSHRunner 适配 target.Service)。
type Runner interface {
	Servers(ctx context.Context) ([]ServerInfo, error)
	Server(ctx context.Context, id string) (ServerInfo, error)
	Exec(ctx context.Context, serverID string, cmd []string) (*target.ExecResult, error)
}

// SSHRunner 把 target.Service 适配为 Runner。
type SSHRunner struct{ Svc target.Service }

// Servers 列出全部纳管服务器(含 PVE 标记,是否可扫由调用方判断)。
func (r SSHRunner) Servers(ctx context.Context) ([]ServerInfo, error) {
	list, err := r.Svc.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ServerInfo, 0, len(list))
	for _, srv := range list {
		out = append(out, ServerInfo{ID: srv.ID, Name: srv.Name, Host: srv.Host, IsPVE: srv.IsPVENode()})
	}
	return out, nil
}

// Server 取单台;不存在 → ErrServerNotFound。
func (r SSHRunner) Server(ctx context.Context, id string) (ServerInfo, error) {
	srv, err := r.Svc.Get(ctx, id)
	if err != nil {
		return ServerInfo{}, ErrServerNotFound
	}
	return ServerInfo{ID: srv.ID, Name: srv.Name, Host: srv.Host, IsPVE: srv.IsPVENode()}, nil
}

// Exec 在目标机执行命令。
func (r SSHRunner) Exec(ctx context.Context, serverID string, cmd []string) (*target.ExecResult, error) {
	return r.Svc.Exec(ctx, serverID, cmd)
}

// ArtifactStore 是报告存储抽象(artifactstore.Store 满足)。
type ArtifactStore interface {
	Put(r io.Reader) (key string, size int64, err error)
	Open(key string) (io.ReadCloser, error)
}

// 扫描命令的固定构件(具名卷跨扫描复用:trivy 漏洞库缓存 / nuclei 特征模板)。
const (
	trivyCacheVolume  = "pipewright-trivy-cache:/root/.cache/trivy"
	nucleiTmplVolume  = "pipewright-nuclei-templates:/root/nuclei-templates"
	dockerSockVolume  = "/var/run/docker.sock:/var/run/docker.sock"
	hostRootsVolume   = "/:/rootfs:ro"
	// maxImagesPerScan 是单次扫描的镜像数上限(docker ps 顺序取前 N 个,防巨型机拖垮)。
	maxImagesPerScan = 20
	// maxNucleiTargets 是单次 nuclei 扫描的目标 URL 上限。
	maxNucleiTargets = 10
)

// StartScan 对单台服务器发起一次扫描:校验入参 → 选中心 trivy 服务(trivy)→
// 落 running 行 → goroutine 异步执行,立即返回。
func (s *service) StartScan(ctx context.Context, in ScanInput) (*Run, error) {
	if s.runner == nil || s.artifacts == nil {
		return nil, ErrRunnerUnavailable
	}
	if !validScanType(in.ScanType) {
		return nil, fmt.Errorf("%w: scan type", ErrInvalidInput)
	}
	if strings.TrimSpace(in.ServerID) == "" {
		return nil, fmt.Errorf("%w: serverId", ErrInvalidInput)
	}
	switch in.Trigger {
	case "", TriggerManual, TriggerFleet, TriggerScheduled:
	default:
		return nil, fmt.Errorf("%w: trigger", ErrInvalidInput)
	}
	targets, err := normalizeNucleiTargets(in.Targets)
	if err != nil {
		return nil, err
	}
	srv, err := s.runner.Server(ctx, in.ServerID)
	if err != nil {
		return nil, err
	}
	if srv.IsPVE {
		return nil, ErrServerNotTarget
	}
	trigger := in.Trigger
	if trigger == "" {
		trigger = TriggerManual
	}
	run := &Run{
		ID: uuid.NewString(), ServerID: srv.ID, ServerName: srv.Name,
		ScanType: in.ScanType, Status: StatusRunning, Trigger: trigger,
		StartedAt: time.Now().UTC(),
	}
	// trivy 优先走最近登记的中心服务(免目标机自下漏洞库);无登记 → 本地缓存卷模式。
	// nuclei 优先走登记的常驻 runner(目标机无需 docker);无登记 → 目标机本地 docker。
	if in.ScanType == ScanTypeTrivy {
		if inst := s.pickTrivyServer(ctx); inst != nil {
			run.ScannerID = inst.ID
		}
	} else if in.ScanType == ScanTypeNuclei {
		if inst := s.pickScanner(ctx, KindNucleiRunner); inst != nil {
			run.ScannerID = inst.ID
		}
	}
	if err := s.insertRun(ctx, run); err != nil {
		return nil, err
	}
	go s.executeAsync(run, srv.Host, targets)
	return run, nil
}

// StartFleetScan 对全部纳管服务器(排除 PVE)按所选类型各发起一次扫描。
func (s *service) StartFleetScan(ctx context.Context, scanTypes []string, trigger string) ([]Run, error) {
	if s.runner == nil || s.artifacts == nil {
		return nil, ErrRunnerUnavailable
	}
	types := normalizeScanTypes(scanTypes)
	if len(types) == 0 {
		return nil, fmt.Errorf("%w: 至少选择一种扫描类型", ErrInvalidInput)
	}
	if trigger == "" {
		trigger = TriggerFleet
	}
	servers, err := s.runner.Servers(ctx)
	if err != nil {
		return nil, err
	}
	runs := make([]Run, 0)
	for _, srv := range servers {
		if srv.IsPVE {
			continue
		}
		for _, st := range types {
			run, err := s.StartScan(ctx, ScanInput{ServerID: srv.ID, ScanType: st, Trigger: trigger})
			if err != nil {
				return nil, err
			}
			runs = append(runs, *run)
		}
	}
	return runs, nil
}

// pickScanner 选最近登记的指定类型扫描器(无 → nil)。ListScanners 已按创建时间倒序。
func (s *service) pickScanner(ctx context.Context, kind string) *Instance {
	list, err := s.ListScanners(ctx)
	if err != nil {
		return nil
	}
	for i := range list {
		if list[i].Kind == kind {
			return &list[i]
		}
	}
	return nil
}

// pickTrivyServer 选最近登记的 trivy-server(无 → nil = 客户端本地缓存卷模式)。
func (s *service) pickTrivyServer(ctx context.Context) *Instance {
	return s.pickScanner(ctx, KindTrivyServer)
}

// nucleiRunnerPSCmd 构造「在 runner 主机上按栈名找执行容器」的 docker ps 命令。
func nucleiRunnerPSCmd(stackName string) []string {
	return []string{"docker", "ps", "--filter", "name=" + stackName + "-nuclei-runner", "--format", "{{.Names}}"}
}

// nucleiNoDockerMsg 是目标机无 docker 且未登记 runner 时的人话指引。
const nucleiNoDockerMsg = "目标机 docker 不可用且未登记 Nuclei Runner:在应用市场部署「Nuclei Runner」(任意有 docker 且与目标网络互通的机器)后,目标机无需 docker 即可扫描"

// executeAsync 是扫描 goroutine 主体:过并发闸 → 执行 → 存报告 → 落终态。
func (s *service) executeAsync(run *Run, host string, nucleiTargets []string) {
	s.sem <- struct{}{}
	defer func() { <-s.sem }()
	ctx, cancel := context.WithCancel(s.baseCtx)
	defer cancel()

	doc, errMsg := s.executeScan(ctx, run, host, nucleiTargets)
	key := ""
	if doc != nil {
		if raw, err := json.Marshal(doc); err == nil {
			k, _, perr := s.artifacts.Put(bytes.NewReader(raw))
			if perr != nil {
				errMsg = strings.TrimSpace(errMsg + ";报告存储失败:" + perr.Error())
			} else {
				key = k
			}
		}
	}
	status := StatusDone
	if doc == nil && errMsg != "" {
		status = StatusFailed
	}
	// 终态落库用独立短超时(扫描 ctx 可能已取消,终态必须写回)。
	fctx, fcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer fcancel()
	summary := Summary{}
	if doc != nil {
		summary = Summarize(doc.Findings)
		summary.ImageCount = doc.imageCount
		summary.TargetCount = len(nucleiTargets)
	}
	_ = s.finishRun(fctx, run.ID, status, summary, key, errMsg)
}

// executeScan 按类型执行,返回 (报告, 错误)。doc 非 nil 即算有产出(可带 errMsg
// 提示部分失败);两者皆空/仅错误 → failed。
func (s *service) executeScan(ctx context.Context, run *Run, host string, nucleiTargets []string) (*ReportDoc, string) {
	switch run.ScanType {
	case ScanTypeTrivy:
		return s.runTrivy(ctx, run, host)
	case ScanTypeNuclei:
		return s.runNuclei(ctx, run, host, nucleiTargets)
	default:
		return nil, "未知扫描类型"
	}
}

// trivyCmd 构造一条目标机 trivy 容器命令(卷挂载 + 可选 DB 镜像仓库 env + 子命令)。
func (s *service) trivyCmd(extraVolume string, args ...string) []string {
	cmd := []string{"docker", "run", "--rm"}
	if s.trivyDBRepo != "" {
		cmd = append(cmd, "-e", "TRIVY_DB_REPOSITORY="+s.trivyDBRepo)
	}
	cmd = append(cmd, "-v", extraVolume, "-v", trivyCacheVolume, s.trivyImage)
	return append(cmd, args...)
}

// tailClip 取文本末尾 n 个字符(失败原因通常在输出末尾,如 trivy 的 FATAL 行;
// 开头是拉镜像进度,从头截会把真原因截掉)。前缀 … 标记截断。
func tailClip(txt string, n int) string {
	txt = strings.TrimSpace(txt)
	r := []rune(txt)
	if len(r) <= n {
		return txt
	}
	return "…" + string(r[len(r)-n:])
}

// dockerUnavailable 判定目标机 docker 不可用(未安装/守护进程未运行/无权限)。
func dockerUnavailable(res *target.ExecResult, err error) bool {
	if res == nil {
		return false
	}
	if res.ExitCode == 127 {
		return true
	}
	stderr := strings.ToLower(res.Stderr)
	return strings.Contains(stderr, "docker: not found") ||
		strings.Contains(stderr, "command not found") ||
		strings.Contains(stderr, "cannot connect to the docker daemon") ||
		strings.Contains(stderr, "permission denied while trying to connect")
}

const dockerUnavailableMsg = "目标机 docker 不可用(未安装、守护进程未运行或当前 SSH 用户无权访问);安全扫描依赖目标机 docker 运行扫描器容器"

// runTrivy 执行 trivy 扫描:① 主机 rootfs(已装系统包 CVE);② 运行中容器镜像
// (docker ps 枚举,逐镜像)。部分失败不整体报废(notes 记录);全部失败 → failed。
func (s *service) runTrivy(ctx context.Context, run *Run, host string) (*ReportDoc, string) {
	serverArgs := []string{}
	if run.ScannerID != "" {
		if inst, err := s.GetScanner(ctx, run.ScannerID); err == nil {
			var sealed []byte
			_ = s.db.QueryRowContext(ctx,
				`SELECT token_sealed FROM scanner_instances WHERE id = ?`, inst.ID).Scan(&sealed)
			if token, terr := s.openToken(sealed); terr == nil && token != "" {
				serverArgs = []string{"--server", "http://" + inst.Host + ":" + fmt.Sprint(inst.Port), "--token", token}
			} else if terr == nil {
				serverArgs = []string{"--server", "http://" + inst.Host + ":" + fmt.Sprint(inst.Port)}
			}
		}
	}
	notes := make([]string, 0)
	findings := make([]Finding, 0)
	succeeded := 0
	imageCount := 0

	// ① 主机包扫描。本地缓存卷模式下客户端要自下漏洞库:配置了镜像仓库(如
	// ghcr.nju.edu.cn)就注入 TRIVY_DB_REPOSITORY,免目标机直连 ghcr.io。
	hostCmd := s.trivyCmd(hostRootsVolume, "rootfs", "-q", "--scanners", "vuln", "--format", "json")
	hostCmd = append(hostCmd, serverArgs...)
	hostCmd = append(hostCmd, "/rootfs")
	res, err := s.runner.Exec(ctx, run.ServerID, hostCmd)
	switch {
	case err != nil:
		return nil, "SSH 执行失败:" + truncateRunes(err.Error(), 256)
	case dockerUnavailable(res, nil):
		return nil, dockerUnavailableMsg
	case res.ExitCode != 0:
		notes = append(notes, "主机包扫描失败:"+tailClip(res.Stderr, 240))
	default:
		if fs, perr := TrivyFindings([]byte(res.Stdout), "host"); perr == nil {
			findings = append(findings, fs...)
			succeeded++
		} else {
			notes = append(notes, "主机包结果解析失败:"+truncateRunes(perr.Error(), 120))
		}
	}

	// ② 运行中镜像扫描。
	psRes, err := s.runner.Exec(ctx, run.ServerID, []string{"docker", "ps", "--format", "{{.Image}}"})
	if err == nil && psRes.ExitCode == 0 {
		seen := map[string]bool{}
		images := make([]string, 0)
		for _, line := range strings.Split(psRes.Stdout, "\n") {
			img := strings.TrimSpace(line)
			if img == "" || img == "<none>" || seen[img] {
				continue
			}
			seen[img] = true
			images = append(images, img)
			if len(images) >= maxImagesPerScan {
				notes = append(notes, fmt.Sprintf("运行镜像超过 %d 个,仅扫描前 %d 个", maxImagesPerScan, maxImagesPerScan))
				break
			}
		}
		for _, img := range images {
			cmd := s.trivyCmd(dockerSockVolume, "image", "-q", "--format", "json")
			cmd = append(cmd, serverArgs...)
			cmd = append(cmd, img)
			ires, ierr := s.runner.Exec(ctx, run.ServerID, cmd)
			if ierr != nil || ires.ExitCode != 0 {
				stderr := ""
				if ires != nil {
					stderr = ires.Stderr
				} else {
					stderr = ierr.Error()
				}
				notes = append(notes, "镜像 "+img+" 扫描失败:"+tailClip(stderr, 200))
				continue
			}
			if fs, perr := TrivyFindings([]byte(ires.Stdout), "image:"+img); perr == nil {
				findings = append(findings, fs...)
				succeeded++
				imageCount++
			}
		}
	} else if err == nil {
		notes = append(notes, "枚举运行镜像失败:"+truncateRunes(strings.TrimSpace(psRes.Stderr), 160))
	}

	if succeeded == 0 {
		msg := strings.Join(notes, ";")
		if msg == "" {
			msg = "trivy 扫描未产生任何结果"
		}
		return nil, truncateRunes(msg, 512)
	}
	SortFindings(findings)
	doc := &ReportDoc{
		ScanType: ScanTypeTrivy, GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Host: host, ServerName: run.ServerName, Findings: findings, Notes: notes,
		imageCount: imageCount,
	}
	if len(notes) > 0 {
		return doc, "扫描完成,但部分对象失败:" + strings.Join(notes, ";")
	}
	return doc, ""
}

// runNuclei 执行 Nuclei 扫描:登记了常驻 runner → 经 runner 主机 docker exec 在容器内
// 跑(目标机无需 docker;扫描视角 = runner 所在网络);否则回落目标机本地 docker run
//(无常驻,特征模板落具名卷复用)。目标为空时默认探测宿主的 80/443。退出非零但已有
// 结果 → 视为部分成功。
func (s *service) runNuclei(ctx context.Context, run *Run, host string, targets []string) (*ReportDoc, string) {
	if len(targets) == 0 {
		targets = []string{"http://" + host, "https://" + host}
	}
	if inst := s.pickScanner(ctx, KindNucleiRunner); inst != nil && inst.ServerID != "" && run.ScannerID == inst.ID {
		return s.runNucleiViaRunner(ctx, run, inst, targets)
	}
	cmd := []string{"docker", "run", "--rm", "-v", nucleiTmplVolume, s.nucleiImage,
		"-j", "-silent", "-nc"}
	for _, t := range targets {
		cmd = append(cmd, "-u", t)
	}
	res, err := s.runner.Exec(ctx, run.ServerID, cmd)
	if err != nil {
		return nil, "SSH 执行失败:" + truncateRunes(err.Error(), 256)
	}
	if dockerUnavailable(res, nil) {
		return nil, nucleiNoDockerMsg
	}
	findings, _ := NucleiFindings([]byte(res.Stdout))
	if res.ExitCode != 0 && len(findings) == 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = fmt.Sprintf("nuclei 以状态码 %d 退出且无结果", res.ExitCode)
		}
		return nil, "nuclei 扫描失败:" + truncateRunes(msg, 400)
	}
	SortFindings(findings)
	doc := &ReportDoc{
		ScanType: ScanTypeNuclei, GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Host: host, ServerName: run.ServerName, Findings: findings,
	}
	if res.ExitCode != 0 {
		doc.Notes = []string{"nuclei 退出码非零,以下为已取得的检测结果(可能不完整):" + truncateRunes(strings.TrimSpace(res.Stderr), 200)}
		return doc, "nuclei 退出码非零,结果可能不完整"
	}
	return doc, ""
}

// runNucleiViaRunner 经常驻 runner 执行:① docker ps 定位执行容器(按栈名前缀);
// ② docker exec 容器内跑 nuclei(特征模板在 runner 容器卷里,跨扫描复用)。
func (s *service) runNucleiViaRunner(ctx context.Context, run *Run, inst *Instance, targets []string) (*ReportDoc, string) {
	psRes, err := s.runner.Exec(ctx, inst.ServerID, nucleiRunnerPSCmd(inst.StackName))
	if err != nil {
		return nil, "连接 runner 所在主机失败:" + truncateRunes(err.Error(), 256)
	}
	if psRes.ExitCode != 0 || dockerUnavailable(psRes, nil) {
		return nil, "runner 所在主机 docker 不可用:" + tailClip(psRes.Stderr, 200)
	}
	container := ""
	for _, line := range strings.Split(psRes.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			container = line
			break
		}
	}
	if container == "" {
		return nil, "runner 容器未在运行(栈 " + inst.StackName + "):可在应用市场重新部署 Nuclei Runner"
	}
	cmd := []string{"docker", "exec", container, "nuclei", "-j", "-silent", "-nc"}
	for _, t := range targets {
		cmd = append(cmd, "-u", t)
	}
	res, err := s.runner.Exec(ctx, inst.ServerID, cmd)
	if err != nil {
		return nil, "经 runner 执行失败:" + truncateRunes(err.Error(), 256)
	}
	if dockerUnavailable(res, nil) {
		return nil, nucleiNoDockerMsg
	}
	findings, _ := NucleiFindings([]byte(res.Stdout))
	if res.ExitCode != 0 && len(findings) == 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = fmt.Sprintf("nuclei 以状态码 %d 退出且无结果", res.ExitCode)
		}
		return nil, "nuclei 扫描失败:" + tailClip(msg, 400)
	}
	SortFindings(findings)
	doc := &ReportDoc{
		ScanType: ScanTypeNuclei, GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Host: "", ServerName: run.ServerName, Findings: findings,
	}
	if res.ExitCode != 0 {
		doc.Notes = []string{"nuclei 退出码非零,以下为已取得的检测结果(可能不完整):" + tailClip(res.Stderr, 200)}
		return doc, "nuclei 退出码非零,结果可能不完整"
	}
	return doc, ""
}

// normalizeNucleiTargets 校验/归一 nuclei 目标 URL:空 → nil(执行时默认
// http(s)://<宿主地址>);每条须为 http(s):// 开头、无空白/控制字符、≤512B;
// 超出上限报错(fail fast,不静默截断用户输入)。
func normalizeNucleiTargets(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(in))
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if len(out) >= maxNucleiTargets {
			return nil, fmt.Errorf("%w: 目标 URL 最多 %d 个", ErrInvalidInput, maxNucleiTargets)
		}
		if len(t) > 512 || !strings.HasPrefix(t, "http://") && !strings.HasPrefix(t, "https://") ||
			strings.ContainsAny(t, " \t\r\n\x00\"'`\\") {
			return nil, fmt.Errorf("%w: 目标 URL 非法(须为 http(s):// 开头且不含空白/引号)", ErrInvalidInput)
		}
		out = append(out, t)
	}
	return out, nil
}
