package deploy

// image_release.go 把 image 产物也纳入「蓝绿(stage→cutover→回滚)」编排(Story 8-8 续 / FR-8-8):
//
// 容器无文件软链可切,故 image 蓝绿用「**全机先 pull(预备,不切换)→ 全机统一停旧起新(切换)→
// 切换阶段任一机健康失败则把已切换成功的机回滚到上一镜像**」的语义:
//   - stageImageOne   : docker pull 新镜像(只拉,不停旧容器)+ 探测当前容器镜像 **digest**(回滚目标)。
//   - activateImageOne: docker rm -f 旧容器 + docker run 新镜像 + 切后健康门控;失败回滚到上一镜像。
//
// 超时预算:部署命令(rm/run)用 60s execCtx;docker pull 走独立长超时(pullTimeout,大镜像
// 慢链路 60s 拉不完);健康门控按 hc 推导独立预算(重试矩阵可远超 60s);回滚用新的 execTimeout
// 级 ctx —— 健康耗尽后旧 execCtx 已过期,复用会让回滚第一条命令就 DeadlineExceeded(坏容器还在跑)。
//
// 与 release 文件模式一致的安全/语义:命令 array 化(不拼 shell)、错误不上抛(映射 status+人读)、
// message 无明文密钥。非 proxy 流量切分(单二进制轻量定位不引入 LB);提供机群一致切换 + 回滚。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/target"
)

// pullTimeout 是 docker pull 的独立长超时:对齐 uploadTimeout(deploy.go,15min)的
// 「大传输不占 60s 命令预算」约定 —— 大镜像经慢链路 60s 必然拉不完。
const pullTimeout = 15 * time.Minute

// imageState 是一台机的 image 蓝绿中间态(stageImageOne 产出,activateImageOne 消费)。
type imageState struct {
	name      string       // 容器名(cfg["containerName"] 优先,否则 sanitizeName(产物名))
	ref       string       // 本次新镜像 ref(repo:tag / image id)
	prevImage string       // 切换前容器所用镜像 **digest**(sha256:…;回滚目标 —— 可变 tag 会被本次 pull 改指,只有 digest 能钉住旧镜像;"" = 无上一容器/读失败,无可回滚)
	baseArgs  []string     // docker run 的附加参数(除端口映射外;结构化键 + cfg["runArgs"] 解析,见 imageBaseRunArgs)
	portSpecs []portSpec   // 端口映射意图(cfg["ports"];自动项运行时分配)
	ports     []portBinding // 本次启动的实际端口绑定(健康检查/注册/回滚复用;启动前为空)
}

// imageContainerName 取部署容器名:cfg["containerName"] 显式优先(净化),否则产物名净化(空 → app)。
func imageContainerName(a run.Artifact, cfg map[string]string) string {
	if c := sanitizeName(strings.TrimSpace(cfg["containerName"])); c != "" {
		return c
	}
	n := sanitizeName(a.Name)
	if n == "" {
		n = "app"
	}
	return n
}

// imagePortSpecs 解析 cfg["ports"] 为端口映射意图(显式 host:container / 容器端口自动分配 /
// auto:容器端口);非法 → 人读错误。空 = 不发布宿主端口。
func imagePortSpecs(cfg map[string]string) ([]portSpec, error) {
	if cfg == nil {
		return nil, nil
	}
	return parsePortSpecs(cfg["ports"])
}

// imageBaseRunArgs 解析 docker run 的附加参数(端口映射已拆到 portSpecs 运行时解析,见 ports.go):
// 先把结构化常用键翻译为对应 flag,再原样追加 cfg["runArgs"] 自由参数(同名 flag 后写生效,
// runArgs 仍作逃生舱)。各参数原样作为 array 元素(绝不拼接 shell;AC-SEC-02)。
//
// 结构化键(UI 表单字段,免手写 flag):
//   - cpuLimit      → --cpus <v>      (CPU 核数上限,如 "1.5")
//   - memoryLimit   → --memory <v>    (内存硬上限,如 "512m";超限 OOM kill)
//   - restartPolicy → --restart <v>   (no/always/unless-stopped/on-failure)
//   - envVars       → 每行一条 KEY=value 各成一个 -e(整行作单值,值含空格不被拆)
//
// 注意:凭据绝不经此进命令(registry 凭据沿用既有 docker login 模式);此处仅承载运行参数。
func imageBaseRunArgs(cfg map[string]string) []string {
	if cfg == nil {
		return nil
	}
	var out []string
	if v := strings.TrimSpace(cfg["cpuLimit"]); v != "" {
		out = append(out, "--cpus", v)
	}
	if v := strings.TrimSpace(cfg["memoryLimit"]); v != "" {
		out = append(out, "--memory", v)
	}
	if v := strings.TrimSpace(cfg["restartPolicy"]); v != "" {
		out = append(out, "--restart", v)
	}
	for _, line := range strings.Split(cfg["envVars"], "\n") {
		if kv := strings.TrimSpace(line); kv != "" {
			out = append(out, "-e", kv)
		}
	}
	return append(out, strings.Fields(cfg["runArgs"])...)
}

// splitImageList 按逗号 / 空白切分并去空(端口列表用)。
func splitImageList(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	}) {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// dockerRunCmd 组装 `docker run -d --name <name> [runArgs...] <ref>`(各元素独立,不拼 shell)。
func dockerRunCmd(name string, runArgs []string, ref string) []string {
	cmd := []string{"docker", "run", "-d", "--name", name}
	cmd = append(cmd, runArgs...)
	cmd = append(cmd, ref)
	return cmd
}

// stageImageOne 执行 image 部署**预备阶段**:docker login(可选,配了 registryCredentialId)+
// pull 新镜像(不停旧容器)+ 探测当前容器镜像(回滚目标)。拉取失败 → (中间态, 人读 message, false)。
func (s *service) stageImageOne(ctx context.Context, srv *target.Server, a run.Artifact, cfg map[string]string) (imageState, string, bool) {
	st := imageState{name: imageContainerName(a, cfg), ref: strings.TrimSpace(a.Reference), baseArgs: imageBaseRunArgs(cfg)}
	if st.ref == "" {
		return st, "image 产物缺少 reference(repo:tag 或镜像 id)", false
	}
	specs, perr := imagePortSpecs(cfg)
	if perr != nil {
		return st, perr.Error(), false
	}
	st.portSpecs = specs
	execCtx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()

	// 私有仓库:配了 registryCredentialId → 先在目标机 docker login(明文仅 target 层经 vault
	// 即取即用,不落日志;失败仅记日志继续,pull 自身可能走匿名缓存)。
	if cred := strings.TrimSpace(cfg["registryCredentialId"]); cred != "" {
		if lerr := s.targets.DockerLogin(ctx, srv.ID, cfg["registryUrl"], cred); lerr != nil {
			cmdLogFrom(ctx)(cmdStreamStderr, "", "  ⚠ 目标机 docker login 失败(继续尝试 pull):"+humanExecError(lerr))
		}
	}

	// 探测当前同名容器所用镜像 digest(供回滚);无容器 / 读失败 → 空(首次部署,无可回滚)。
	// 必须在 pull 之前读:pull 之后可变 tag 已改指新镜像,引用串不再是有效回滚目标。
	st.prevImage = s.readContainerImage(execCtx, srv.ID, st.name)

	// 仅 pull,不动旧容器(零中断预备)。pull 走独立长超时(pullTimeout),不占 60s 命令预算。
	pullCtx, pullCancel := context.WithTimeout(ctx, pullTimeout)
	failMsg, ok := s.runStep(pullCtx, srv.ID, [][]string{{"docker", "pull", st.ref}})
	pullCancel()
	if !ok {
		return st, failMsg, false
	}
	return st, "", true
}

// deployImageOne 执行**滚动 / 金丝雀**单机 image 部署:pull 新镜像(捕获上一镜像作回滚目标)→
// 停旧起新 + 切后健康门控 → 健康失败回滚到上一镜像。复用蓝绿单机原语 stageImageOne/activateImageOne,
// 但每机独立(无机群协调)。返回结果 + 最终中间态(供调用方取实际端口绑定做实例注册)。
func (s *service) deployImageOne(ctx context.Context, srv *target.Server, a run.Artifact, cfg map[string]string, hsp *healthSpec, started time.Time) (TargetResult, imageState) {
	st, failMsg, ok := s.stageImageOne(ctx, srv, a, cfg)
	if !ok {
		return finishFailed(TargetResult{ServerID: srv.ID, ServerName: srv.Name, StartedAt: started}, failMsg), st
	}
	return s.activateImageOne(ctx, srv, a, cfg, hsp, st, started, "")
}

// activateImageOne 执行 image **切换阶段**:停旧容器 + 起新镜像容器(自动端口分配 + 竞态重试)
// + 切后健康门控(端口自动推导:首个映射的宿主端口)+ 失败回滚到上一镜像(复用同端口绑定,
// 服务地址稳定)。modeLabel 仅用于成功文案区分编排策略("蓝绿" / 空=滚动/金丝雀)。
// 返回结果 + **含实际端口绑定的最终中间态**(回滚路径同样复用该绑定,宿主端口不变,
// 调用方据 final.ports 做实例注册才不会丢 hostPort)。
func (s *service) activateImageOne(ctx context.Context, srv *target.Server, a run.Artifact, cfg map[string]string, hsp *healthSpec, st imageState, started time.Time, modeLabel string) (TargetResult, imageState) {
	res := TargetResult{ServerID: srv.ID, ServerName: srv.Name, StartedAt: started}
	execCtx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()

	// 切换:移除同名旧容器(幂等)→ 起新镜像容器(端口分配 + docker 仲裁重试)。
	// 失败一律走 rollbackImage 并传**父 ctx**(rollbackImage 内部挂独立预算;此处 execCtx
	// 可能已被前面的命令消耗)。
	if failMsg, ok := s.runStep(execCtx, srv.ID, [][]string{{"docker", "rm", "-f", st.name}}); !ok {
		return s.rollbackImage(ctx, srv, res, st, failMsg), st
	}
	bindings, runMsg, ok := s.runContainerWithPorts(execCtx, srv.ID, st.name, st.baseArgs, st.portSpecs, cfg, st.ref)
	if !ok {
		// 起新容器失败:尽力回滚到上一镜像(与健康失败同语义),避免目标机被留在坏状态。
		return s.rollbackImage(ctx, srv, res, st, runMsg), st
	}
	st.ports = bindings

	// 切后健康门控;失败触发回滚到上一镜像(回滚 rm 容器前先回捞日志,保住失败现场)。
	// 端口自动推导:仅填 healthPath 时取首个映射的宿主端口。
	// 门控用独立预算 ctx(重试矩阵可远超 60s execCtx;否则重试没跑完就被砍,还拖死回滚)。
	hc := hsp.resolve(firstHostPort(st.ports))
	if hc.enabled() {
		hcCtx, hcCancel := healthCheckCtx(ctx, hc)
		herr := s.runHealthCheck(hcCtx, srv.ID, hc)
		hcCancel()
		if herr != nil {
			s.dumpContainerLogs(ctx, srv.ID, st.name)
			return s.rollbackImage(ctx, srv, res, st, herr.Error()), st
		}
	}

	finish := time.Now().UTC()
	res.Status = run.TargetSuccess
	portNote := ""
	if len(st.ports) > 0 {
		portNote = ",端口 " + portBindingSummary(st.ports)
	}
	if hc.enabled() {
		res.Message = fmt.Sprintf("image %s部署完成 → 容器 %s(%s%s,健康检查通过)", modeLabel, st.name, st.ref, portNote)
	} else {
		res.Message = fmt.Sprintf("image %s部署完成 → 容器 %s(%s%s)", modeLabel, st.name, st.ref, portNote)
	}
	res.FinishedAt = &finish
	return res, st
}

// rollbackImage 在健康失败后把容器回滚到上一镜像 digest(rm 新容器 → run 上一镜像,**复用本次的
// 端口绑定** —— 宿主端口不变,网关 upstream / 防火墙规则无需跟随)。
// ctx 传调用方的**父 ctx**:回滚命令内部挂独立 execTimeout 级预算 —— 健康门控耗尽后切换阶段的
// execCtx 已过期,复用它会让第一条 docker rm 就 DeadlineExceeded(坏容器还在跑,状态却记rolled_back)。
// 无上一镜像(首次部署)→ failed;回滚命令失败 → **failed**(rolled_back 语义是「仍运行旧版本」,
// 回滚未确认时机器上可能已没有容器在跑,记 rolled_back 是撒谎;failed 仍可被 RetryFailed 推进)。
func (s *service) rollbackImage(ctx context.Context, srv *target.Server, res TargetResult, st imageState, healthMsg string) TargetResult {
	finish := time.Now().UTC()
	res.FinishedAt = &finish
	if st.prevImage == "" {
		res.Status = run.TargetFailed
		res.Message = fmt.Sprintf("健康检查失败且无上一镜像可回滚(首次部署):%s", healthMsg)
		return res
	}
	rbCtx, rbCancel := context.WithTimeout(ctx, execTimeout)
	defer rbCancel()
	rollbackArgs := append(portFlagArgs(st.ports), st.baseArgs...)
	// 回滚失败 = 执行错误 **或非零退出**(run 非零退出时机器上已无容器在跑,绝非「仍运行旧版本」)。
	rbFail := ""
	for _, cmd := range [][]string{
		{"docker", "rm", "-f", st.name},
		dockerRunCmd(st.name, rollbackArgs, st.prevImage),
	} {
		out, e := s.exec(rbCtx, srv.ID, cmd)
		if e != nil {
			rbFail = humanExecError(e)
			break
		}
		if out != nil && out.ExitCode != 0 {
			rbFail = fmt.Sprintf("回滚命令退出码 %d:%s", out.ExitCode, truncate(strings.TrimSpace(out.Stderr)))
			break
		}
	}
	if rbFail != "" {
		res.Status = run.TargetFailed
		res.Message = fmt.Sprintf("健康检查失败,已尝试回滚容器 %s → 上一镜像 %s,但回滚命令失败(回滚未确认,机器上可能无容器在跑):%s(健康原因:%s)",
			st.name, st.prevImage, rbFail, healthMsg)
		return res
	}
	res.Status = run.TargetRolledBack
	res.Message = fmt.Sprintf("健康检查失败,已回滚容器 %s → 上一镜像 %s(健康原因:%s)", st.name, st.prevImage, healthMsg)
	return res
}

// readContainerImage 读同名容器当前所用镜像的 **digest**(`{{.Image}}`,形如 sha256:…)作为
// 回滚目标:可变 tag(如 app:latest)在本次 docker pull 后已改指新镜像,拿引用串(`{{.Config.Image}}`)
// 回滚等于把刚拉的坏镜像再跑一遍(假回滚);只有 digest 能钉住切换前真正在跑的镜像。
// 无容器 / 读失败 → ""(无可回滚,按首次部署处理)。
func (s *service) readContainerImage(ctx context.Context, serverID, name string) string {
	out, err := s.exec(ctx, serverID, []string{"docker", "inspect", "--format", "{{.Image}}", name})
	if err != nil || out == nil || out.ExitCode != 0 {
		return ""
	}
	return strings.TrimSpace(out.Stdout)
}
