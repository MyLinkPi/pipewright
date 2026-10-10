package deploy

// instance_rolling.go 实现容器实例的「扩容升级缩容」单机执行序(maxSurge 轮转):配合服务
// 注册网关(nginx)的集群实例池,把一次 image 部署升级为逐实例零停机替换。
//
// 单实例执行序(k8s maxSurge 语义):
//
//	1. docker pull 新镜像(预备,不动旧实例)
//	2. docker run 起一个**新名字**的实例容器 —— 宿主端口范围探测自动分配(旧容器占用的口
//	   自然被探测跳过);旧实例继续服务,新实例先不进 upstream
//	3. 预热健康:对新实例容器探测(切流量**之前**验证;走宿主映射端口,与切后门控/网关
//	   upstream 同一条链路)
//	4. SwapInstance:upstream 成员原子替换(旧出、新进 + host_port 同步,一次 reload)——
//	   新实例已健康、旧实例还在跑,任何时刻不为零后端
//	5. 排空窗口(drainSeconds,等存量请求;OSS nginx 无逐连接排空,固定等待)
//	6. docker rm -f 旧实例容器(缩容)
//
// 失败语义:步骤 2/3/4 失败 → 回捞新容器日志 + 删除新容器(收尾挂部署父 ctx 的独立预算,
// rollCtx 被健康重试矩阵耗尽时仍能执行)、该实例保留旧版本、该机 failed 并**中止其余实例**
// (未动实例保持旧版本);步骤 6 失败仅记告警(已切换,残留旧容器无害)。
// 天然无需回滚:旧实例从头到尾未停止服务。
//
// 注册联动:cfg["regServiceId"](部署节点「一键生成」的绑定;legacy 键 gatewayService 兼容)
// 非空且反查不到实例 → 先 EnsureInstance 落行(首台部署/新扩机器),再进入轮转。
// 非网关托管(未装配 InstanceGateway / 反查不到服务)→ 与既有 rolling 一致的单机部署
// (deployImageOne),成功后 best-effort 落实例行(硬切路径也保持注册表与实况一致)。
//
// 网关交互经 InstanceGateway 小接口由 main 晚绑(deploy 不 import servicereg,保持包间单向依赖)。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/target"
)

// InstanceRef 是实例轮转的一个目标实例(InstanceGateway 返回;deploy 不感知 servicereg 类型)。
type InstanceRef struct {
	ServiceID   string
	ServiceName string // 展示名(FQDN)
	InstanceID  string
	ServerID    string // 实例归属服务器(= 部署目标机)
	Container   string // 旧实例容器名(轮转后由新容器名接替);非容器实例为空
	Port        int    // 实例生效服务端口(容器内应用端口)
	HostPort    int    // 实例生效宿主端口(网关反代地址)
}

// InstanceGateway 抽象服务注册网关的实例级能力:按部署目标机反查实例 + 原子摘挂/换实例 +
// 部署期注册(ensure/prune)。nil(未装配)→ maxSurge 路径回退既有滚动,摘挂路径跳过。
type InstanceGateway interface {
	// ResolveInstances 返回该部署目标机(serverID)上的 attached 实例。serviceRef 是服务
	// 引用(注册绑定的服务 ID,legacy 为服务名);container 是容器名(镜像部署反查键,可空)。
	// 两者至少给一个;匹配不到 → 空切片(调用方回退旧滚动)。
	ResolveInstances(ctx context.Context, serverID, serviceRef, container string) ([]InstanceRef, error)
	// SwapInstance 原子替换实例(单次 reload):upstream 中 old 出、new 进,host_port 同步更新。
	SwapInstance(ctx context.Context, serviceID, serverID, oldContainer, newContainer string, hostPort int) error
	// DetachInstance 把实例从 upstream 摘除 + reload(非容器「摘→升→挂回」的摘)。
	DetachInstance(ctx context.Context, instanceID string) error
	// AttachInstance 把实例挂回 upstream + reload(部署健康通过后恢复流量)。
	AttachInstance(ctx context.Context, instanceID string) error
	// EnsureInstance 幂等 upsert 实例行(port<=0/hostPort<=0 = 存量行保持;部署期注册用)。
	EnsureInstance(ctx context.Context, serviceID, serverID, container string, port, hostPort int) error
	// EnsureInstanceDetached 部署期预注册(摘挂流程第一步):确保实例行存在且本轮部署期间
	// 不进 upstream(新行 detached;存量行只更新端口/归属,attached 保持)。返回实例引用,
	// 供部署成功后挂回。
	EnsureInstanceDetached(ctx context.Context, serviceID, serverID, container string, port, hostPort int) (InstanceRef, error)
	// PruneInstances 删除服务下 server 不在列表内的实例(部署全部成功后同步拓扑)。
	PruneInstances(ctx context.Context, serviceID string, serverIDs []string) error
}

// 排空窗口(秒):默认 / 上限。
const (
	defaultDrainSeconds = 5
	maxDrainSeconds     = 60
	// instanceRollBaseTimeout / perInstance 是整轮实例轮转的预算基数(秒):pull + 每实例
	// (端口分配 + run + 预热健康(含重试)+ swap + drain + rm)。
	instanceRollBaseTimeout     = 3 * time.Minute
	instanceRollPerInstanceTime = 90 * time.Second
)

// WithInstanceGateway 注入实例轮转网关(main 经适配器晚绑 servicereg;nil → 回退旧滚动)。
func WithInstanceGateway(g InstanceGateway) Option {
	return func(s *service) { s.instanceGateway = g }
}

// regServiceRef 取部署节点的服务注册绑定:regServiceId(一键生成)优先,gatewayService(legacy
// 服务名)兜底。
func regServiceRef(cfg map[string]string) string {
	if id := strings.TrimSpace(cfg["regServiceId"]); id != "" {
		return id
	}
	return strings.TrimSpace(cfg["gatewayService"])
}

// deployInstanceRollingOne 单机入口:装配了网关且能反查到服务 → 逐实例轮转;否则回退旧路径。
func (s *service) deployInstanceRollingOne(ctx context.Context, srv *target.Server, a run.Artifact, cfg map[string]string, hsp *healthSpec) TargetResult {
	started := time.Now().UTC()
	st := imageState{
		name:     imageContainerName(a, cfg),
		ref:      strings.TrimSpace(a.Reference),
		baseArgs: imageBaseRunArgs(cfg),
	}
	specs, perr := imagePortSpecs(cfg)
	if perr != nil {
		return finishFailed(TargetResult{ServerID: srv.ID, ServerName: srv.Name, StartedAt: started}, perr.Error())
	}
	st.portSpecs = specs
	if st.ref == "" {
		return finishFailed(TargetResult{ServerID: srv.ID, ServerName: srv.Name, StartedAt: started},
			"image 产物缺少 reference(repo:tag 或镜像 id)")
	}

	serviceRef := regServiceRef(cfg)
	if s.instanceGateway == nil {
		return s.deployImageFallback(ctx, srv, a, cfg, hsp, st, serviceRef, started)
	}
	instances, err := s.instanceGateway.ResolveInstances(ctx, srv.ID, serviceRef, st.name)
	if err != nil || len(instances) == 0 {
		// 首台部署 / 新扩机器:注册绑定存在但本机还没有实例行 → 先落行再轮转(SwapInstance
		// 需要行存在)。仍反查不到(服务不存在等)→ 回退既有单机滚动,行为不变。
		if serviceRef != "" && err == nil {
			if eerr := s.instanceGateway.EnsureInstance(ctx, serviceRef, srv.ID, st.name, firstContainerPort(specs), 0); eerr == nil {
				instances, err = s.instanceGateway.ResolveInstances(ctx, srv.ID, serviceRef, st.name)
			}
		}
		if err != nil || len(instances) == 0 {
			return s.deployImageFallback(ctx, srv, a, cfg, hsp, st, serviceRef, started)
		}
	}

	rollCtx, cancel := context.WithTimeout(ctx, instanceRollBaseTimeout+instanceRollPerInstanceTime*time.Duration(len(instances)))
	defer cancel()

	// 预备:pull 新镜像(不动旧实例)。
	if failMsg, ok := s.runStep(rollCtx, srv.ID, [][]string{{"docker", "pull", st.ref}}); !ok {
		return finishFailed(TargetResult{ServerID: srv.ID, ServerName: srv.Name, StartedAt: started}, failMsg)
	}

	var done int
	for _, inst := range instances {
		if msg, ok := s.rollOneInstance(rollCtx, ctx, srv, st, inst, hsp, cfg); !ok {
			finish := time.Now().UTC()
			return TargetResult{
				ServerID: srv.ID, ServerName: srv.Name,
				Status:    run.TargetFailed,
				Message:   fmt.Sprintf("实例轮转失败(已切 %d/%d 个实例,中止剩余;失败实例 %s 保留旧版本继续服务):%s", done, len(instances), inst.Container, msg),
				StartedAt: started, FinishedAt: &finish,
			}
		}
		done++
	}

	finish := time.Now().UTC()
	return TargetResult{
		ServerID: srv.ID, ServerName: srv.Name,
		Status: run.TargetSuccess,
		Message: fmt.Sprintf("image 实例轮转完成 → 服务 %s:%d 个实例逐个「扩容起新→预热健康→原子切换→排空→缩容停旧」零停机替换(%s)",
			instances[0].ServiceName, len(instances), st.ref),
		StartedAt: started, FinishedAt: &finish,
	}
}

// deployImageFallback 非网关托管路径:既有单机滚动(含 pull/健康/回滚);成功后 best-effort
// 落实例行(注册表与实况一致 —— 硬切路径的宿主端口同样要进集群 upstream)。
func (s *service) deployImageFallback(ctx context.Context, srv *target.Server, a run.Artifact, cfg map[string]string, hsp *healthSpec, st imageState, serviceRef string, started time.Time) TargetResult {
	res, final := s.deployImageOne(ctx, srv, a, cfg, hsp, started)
	if res.Status == run.TargetSuccess && s.instanceGateway != nil && serviceRef != "" {
		if eerr := s.instanceGateway.EnsureInstance(ctx, serviceRef, srv.ID, st.name, firstContainerPort(final.portSpecs), firstHostPort(final.ports)); eerr != nil {
			res.Message += "(网关注册告警:" + humanExecError(eerr) + ")"
		}
	}
	return res
}

// rollOneInstance 替换单个实例(六步序);失败返回 (人读原因, false),调用方中止其余实例。
// rollCtx 是整轮预算(健康重试 / 切换 / 排空都吃它);failCtx 是部署父 ctx —— 失败收尾
// (回捞日志 / 删新容器)必须挂它 + 独立 execTimeout 预算:重型健康重试矩阵可在中途耗尽
// rollCtx,复用会让收尾命令被 DeadlineExceeded 拦住 —— 失败现场丢失、新容器残留
// (与 rollbackImage 的「父 ctx + 独立预算」模式一致)。
func (s *service) rollOneInstance(rollCtx, failCtx context.Context, srv *target.Server, st imageState, inst InstanceRef, hsp *healthSpec, cfg map[string]string) (string, bool) {
	newName := st.name + "-r" + instanceRandSuffix()

	// 1) 扩容:起新实例容器(宿主端口自动分配;旧容器占用的端口被探测自然跳过)。
	bindings, runMsg, ok := s.runContainerWithPorts(rollCtx, srv.ID, newName, st.baseArgs, st.portSpecs, cfg, st.ref)
	if !ok {
		return "起新实例容器失败:" + runMsg, false
	}
	// 失败收尾:删新容器(独立预算,理由见函数注释);返回人读结果供失败消息据实拼接 ——
	// 清理未确认时机器上可能残留新容器,绝不硬编码「已删除」。
	cleanupNew := func() string {
		cctx, cancel := context.WithTimeout(failCtx, execTimeout)
		defer cancel()
		out, err := s.exec(cctx, srv.ID, []string{"docker", "rm", "-f", newName})
		if err != nil {
			return "新容器清理未确认(可能残留 " + newName + "):" + humanExecError(err)
		}
		if out != nil && out.ExitCode != 0 {
			return fmt.Sprintf("新容器清理未确认(可能残留 %s):退出码 %d", newName, out.ExitCode)
		}
		return "已删除新容器"
	}

	// 2) 预热健康:切流量之前验证新实例(宿主映射端口,网关 upstream 将来要打的同一地址)。
	//    失败即删新容器保旧实例;删前回捞容器日志尾部(否则失败现场随容器一起消失)。
	if perr := s.probeInstance(rollCtx, srv, hsp, newName, firstHostPort(bindings)); perr != nil {
		s.dumpContainerLogs(failCtx, srv.ID, newName)
		return "新实例预热健康未通过(" + cleanupNew() + ",旧实例未受影响):" + perr.Error(), false
	}

	// 3) 原子切换:upstream 成员 old 出、new 进 + host_port 同步(一次 reload)。
	if serr := s.instanceGateway.SwapInstance(rollCtx, inst.ServiceID, srv.ID, inst.Container, newName, firstHostPort(bindings)); serr != nil {
		return "切换 upstream 失败(" + cleanupNew() + ",旧实例未受影响):" + serr.Error(), false
	}

	// 4) 排空窗口:等打到旧实例的存量请求结束(固定等待;OSS nginx 无逐连接排空)。
	if d := drainSeconds(cfg); d > 0 {
		t := time.NewTimer(d)
		select {
		case <-t.C:
		case <-rollCtx.Done():
			t.Stop()
		}
	}

	// 5) 缩容:停旧实例容器(已切换;失败仅告警不判败 —— 残留旧容器无害,下轮可清)。
	if out, err := s.exec(rollCtx, srv.ID, []string{"docker", "rm", "-f", inst.Container}); err != nil || (out != nil && out.ExitCode != 0) {
		_, _ = s.exec(rollCtx, srv.ID, []string{"docker", "rm", "-f", inst.Container})
	}
	return "", true
}

// probeInstance 在切流量前验证新实例:
//   - 端口型健康门控:探测 URL = http://127.0.0.1:<新实例宿主端口><path>(与常规部署切后门控
//     同一条 resolve 链,healthcheck.go)。docker run 成功即已绑定映射(iptables/docker-proxy
//     就绪),宿主口是 docker run -p 的对外契约,也是 SwapInstance 之后网关 upstream 打的
//     同一地址 —— 预热就验生产流量真正要走的链路;容器网段直连(IP:容器口)在 rootless docker、
//     防火墙拦 host→docker0 等环境不可达,且与服务地址语义不一致,不再使用;
//   - exec 型(docker exec):目标容器名替换为新实例名再探测 —— spec 构造时烤入的是基础容器名
//     (deploy.go 调用链),直接打会命中旧实例(假通过)或 no such container(必失败);
//   - command/url 型:原样执行(command 在目标机 shell 跑、无容器名可换;语义由用户把握);
//   - 无健康配置:容器 Running settle 检查(短等待 × 3 次)。
func (s *service) probeInstance(ctx context.Context, srv *target.Server, hsp *healthSpec, newContainer string, hostPort int) error {
	if hsp != nil {
		switch hsp.kind {
		case specExec:
			if hc := hsp.resolve(0); hc != nil && hc.enabled() {
				hc.Command = retargetExecContainer(hc.Command, hsp.container, newContainer)
				return s.runHealthCheck(ctx, srv.ID, hc)
			}
		case specCommand, specURL:
			if hc := hsp.resolve(0); hc != nil && hc.enabled() {
				return s.runHealthCheck(ctx, srv.ID, hc)
			}
		case specPort:
			if hc := hsp.resolve(hostPort); hc != nil && hc.enabled() {
				return s.runHealthCheck(ctx, srv.ID, hc)
			}
			// 与切后门控 / 硬切路径(端口推不出时 hc==nil 静默跳过)不同:新实例尚未接流量,
			// 宿主端口推不出来就无法验证,此处硬失败并引导显式配置(不再回退容器网段直连探测;
			// runArgs 自带 -p 时平台无法感知映射值,同样需要显式 healthPort)。
			return errors.New("无法推导新实例宿主端口,预热探测无法执行;请配置端口映射(ports)或显式 healthPort")
		}
	}
	// 无健康检查配置:容器进入运行态的 settle 检查。
	for i := 0; i < 3; i++ {
		out, eerr := s.exec(ctx, srv.ID, []string{"docker", "inspect", "--format", "{{.State.Running}}", newContainer})
		if eerr == nil && out != nil && out.ExitCode == 0 && strings.EqualFold(strings.TrimSpace(out.Stdout), "true") {
			return nil
		}
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fmt.Errorf("新实例容器 %s 未进入运行态", newContainer)
}

// drainSeconds 解析排空窗口(cfg["drainSeconds"];默认 5s,夹 0..60)。
func drainSeconds(cfg map[string]string) time.Duration {
	n := defaultDrainSeconds
	if raw := strings.TrimSpace(cfg["drainSeconds"]); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil {
			n = v
		}
	}
	if n < 0 {
		n = 0
	}
	if n > maxDrainSeconds {
		n = maxDrainSeconds
	}
	return time.Duration(n) * time.Second
}

// instanceRandSuffix 生成 6 位 hex 后缀(新实例容器名 <name>-r<hex>,唯一且过容器名白名单)。
func instanceRandSuffix() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
