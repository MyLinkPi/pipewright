package deploy

// instance_rolling.go 实现「实例级轮转升级」(默认策略):配合服务注册网关(nginx)的
// 多实例 upstream,把一次 image 部署从「整机硬切(rm 旧 → run 新,窗口内 502)」升级为
// 逐实例零停机替换。
//
// 单实例执行序(k8s maxSurge 语义):
//
//	1. docker pull 新镜像(预备,不动旧实例)
//	2. docker run 起一个**新名字**的实例容器(旧实例继续服务;新实例先不进 upstream)
//	3. 预热健康:对新实例容器探测(切流量**之前**验证)
//	4. SwapInstance:upstream 成员原子替换(旧出、新进,一次 reload)—— 新实例已健康、
//	   旧实例还在跑,任何时刻不为零后端
//	5. 排空窗口(drainSeconds,等存量请求;OSS nginx 无逐连接排空,固定等待)
//	6. docker rm -f 旧实例容器
//
// 失败语义:步骤 2/3/4 失败 → 删除新容器、该实例保留旧版本、该机 failed 并**中止其余实例**
// (未动实例保持旧版本);步骤 6 失败仅记告警(已切换,残留旧容器无害)。
//
// 回退:未装配 InstanceGateway / 反查不到匹配服务 / 非 image 产物 → 与既有 rolling 逐字节
// 一致的单机部署(deployImageOne/deployOne),存量部署行为不变。
//
// 网关交互经 InstanceGateway 小接口由 main 晚绑(deploy 不 import servicereg,保持包间单向依赖)。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/target"
)

// InstanceRef 是实例轮转的一个目标实例(InstanceGateway 返回;deploy 不感知 servicereg 类型)。
type InstanceRef struct {
	ServiceID   string
	ServiceName string // 展示名(FQDN)
	InstanceID  string
	Container   string // 旧实例容器名(轮转后由新容器名接替)
	Port        int    // 实例生效端口
}

// InstanceGateway 抽象服务注册网关的实例级能力:按部署容器名反查网关服务实例 + 原子摘挂。
// nil(未装配)→ instance_rolling 全程回退既有滚动。
type InstanceGateway interface {
	// ResolveInstances 返回(部署容器名所对应的)网关服务的全部 attached 实例;
	// 无匹配 / 目标机不是网关主机 → 空切片(调用方据此回退)。
	ResolveInstances(ctx context.Context, serverID, container string) ([]InstanceRef, error)
	// SwapInstance 原子替换实例(单次 reload):upstream 中 old 出、new 进。
	SwapInstance(ctx context.Context, serviceID, oldContainer, newContainer string) error
}

// 排空窗口(秒):默认 / 上限。
const (
	defaultDrainSeconds = 5
	maxDrainSeconds     = 60
	// instanceRollBaseTimeout / perInstance 是整轮实例轮转的预算基数(秒):pull + 每实例
	// (run + 预热健康(含重试)+ swap + drain + rm)。
	instanceRollBaseTimeout     = 3 * time.Minute
	instanceRollPerInstanceTime = 90 * time.Second
)

// WithInstanceGateway 注入实例轮转网关(main 经适配器晚绑 servicereg;nil → 回退旧滚动)。
func WithInstanceGateway(g InstanceGateway) Option {
	return func(s *service) { s.instanceGateway = g }
}

// deployInstanceRolling 机群扇出(与 deployFanout 同构:有界并发 + 单机 recover);
// 每机独立决策:能反查到网关实例 → 实例轮转;否则回退该机既有部署(deployImageOne/deployOne)。
func (s *service) deployInstanceRolling(ctx context.Context, servers []*target.Server, a run.Artifact, cfg map[string]string, hc *HealthCheck) []TargetResult {
	results := make([]TargetResult, len(servers))
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxParallelDeploys)
	for i, srv := range servers {
		wg.Add(1)
		go func(idx int, srv *target.Server) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					finish := time.Now().UTC()
					results[idx] = TargetResult{
						ServerID: srv.ID, ServerName: srv.Name,
						Status: run.TargetFailed,
						Message: fmt.Sprintf("实例轮转 panic(已恢复,不影响其它机):%v", r),
						StartedAt: finish, FinishedAt: &finish,
					}
				}
			}()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[idx] = s.deployInstanceRollingOne(ctx, srv, a, cfg, hc)
		}(i, srv)
	}
	wg.Wait()
	return results
}

// deployInstanceRollingOne 单机入口:装配了网关且能反查到实例 → 逐实例轮转;否则回退旧路径。
func (s *service) deployInstanceRollingOne(ctx context.Context, srv *target.Server, a run.Artifact, cfg map[string]string, hc *HealthCheck) TargetResult {
	started := time.Now().UTC()
	if s.instanceGateway == nil {
		return s.deployImageOne(ctx, srv, a, cfg, hc, started)
	}
	st := imageState{
		name:     imageContainerName(a, cfg),
		ref:      strings.TrimSpace(a.Reference),
		runArgs:  imageRunArgs(cfg),
	}
	if st.ref == "" {
		return finishFailed(TargetResult{ServerID: srv.ID, ServerName: srv.Name, StartedAt: started},
			"image 产物缺少 reference(repo:tag 或镜像 id)")
	}
	instances, err := s.instanceGateway.ResolveInstances(ctx, srv.ID, st.name)
	if err != nil || len(instances) == 0 {
		// 非网关托管(或反查出错,保守起见)→ 既有单机滚动(含 pull/健康/回滚),行为不变。
		return s.deployImageOne(ctx, srv, a, cfg, hc, started)
	}

	rollCtx, cancel := context.WithTimeout(ctx, instanceRollBaseTimeout+instanceRollPerInstanceTime*time.Duration(len(instances)))
	defer cancel()

	// 预备:pull 新镜像(不动旧实例)。
	if failMsg, ok := s.runStep(rollCtx, srv.ID, [][]string{{"docker", "pull", st.ref}}); !ok {
		return finishFailed(TargetResult{ServerID: srv.ID, ServerName: srv.Name, StartedAt: started}, failMsg)
	}

	var done int
	for _, inst := range instances {
		if msg, ok := s.rollOneInstance(rollCtx, srv, st, inst, hc, cfg); !ok {
			finish := time.Now().UTC()
			return TargetResult{
				ServerID: srv.ID, ServerName: srv.Name,
				Status:  run.TargetFailed,
				Message: fmt.Sprintf("实例轮转失败(已切 %d/%d 个实例,中止剩余;失败实例 %s 保留旧版本继续服务):%s", done, len(instances), inst.Container, msg),
				StartedAt: started, FinishedAt: &finish,
			}
		}
		done++
	}

	finish := time.Now().UTC()
	return TargetResult{
		ServerID: srv.ID, ServerName: srv.Name,
		Status: run.TargetSuccess,
		Message: fmt.Sprintf("image 实例轮转完成 → 服务 %s:%d 个实例逐个「起新→预热健康→原子切换→排空→停旧」零停机替换(%s)",
			instances[0].ServiceName, len(instances), st.ref),
		StartedAt: started, FinishedAt: &finish,
	}
}

// rollOneInstance 替换单个实例(五步序);失败返回 (false, 人读原因),调用方中止其余实例。
func (s *service) rollOneInstance(ctx context.Context, srv *target.Server, st imageState, inst InstanceRef, hc *HealthCheck, cfg map[string]string) (string, bool) {
	newName := st.name + "-r" + instanceRandSuffix()

	// 1) 起新实例容器(旧实例继续服务)。
	out, err := s.exec(ctx, srv.ID, dockerRunCmd(newName, st.runArgs, st.ref))
	if err != nil {
		return humanExecError(err), false
	}
	if out != nil && out.ExitCode != 0 {
		msg := truncate(strings.TrimSpace(firstString(out.Stderr, out.Stdout)))
		if strings.Contains(strings.ToLower(out.Stderr+out.Stdout), "port is already allocated") {
			msg = "宿主端口冲突;网关托管实例不应发布宿主端口(-p,流量经 nginx 共享网络进实例)—— " + msg
		}
		return "起新实例容器失败:" + msg, false
	}
	cleanupNew := func() {
		_, _ = s.exec(ctx, srv.ID, []string{"docker", "rm", "-f", newName})
	}

	// 2) 预热健康:切流量之前验证新实例。
	if perr := s.probeInstance(ctx, srv, inst, hc, newName); perr != nil {
		cleanupNew()
		return "新实例预热健康未通过(已删除新容器,旧实例未受影响):" + perr.Error(), false
	}

	// 3) 原子切换:upstream 成员 old 出、new 进(一次 reload)。
	if serr := s.instanceGateway.SwapInstance(ctx, inst.ServiceID, inst.Container, newName); serr != nil {
		cleanupNew()
		return "切换 upstream 失败(已删除新容器,旧实例未受影响):" + serr.Error(), false
	}

	// 4) 排空窗口:等打到旧实例的存量请求结束(固定等待;OSS nginx 无逐连接排空)。
	if d := drainSeconds(cfg); d > 0 {
		t := time.NewTimer(d)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
		}
	}

	// 5) 停旧实例容器(已切换;失败仅告警不判败 —— 残留旧容器无害,下轮可清)。
	if out, err := s.exec(ctx, srv.ID, []string{"docker", "rm", "-f", inst.Container}); err != nil || (out != nil && out.ExitCode != 0) {
		_, _ = s.exec(ctx, srv.ID, []string{"docker", "rm", "-f", inst.Container})
	}
	return "", true
}

// probeInstance 在切流量前验证新实例:
//   - hc=http   :探测 URL 重写为 scheme://<容器IP>:<实例端口><path+query>(取容器 IP,宿主 curl
//     直达容器网段;此时新容器尚未接共享网络,用其默认网络 IP 即可);
//   - hc=command:原样执行(语义由用户把握);
//   - 无 hc     :容器 Running settle 检查(短等待 × 3 次)。
func (s *service) probeInstance(ctx context.Context, srv *target.Server, inst InstanceRef, hc *HealthCheck, newContainer string) error {
	if hc != nil && hc.Type == HealthCheckHTTP && strings.TrimSpace(hc.URL) != "" {
		ip := s.containerIP(ctx, srv.ID, newContainer)
		if ip == "" {
			return errors.New("取新实例容器 IP 失败(docker inspect Networks)")
		}
		u, perr := url.Parse(strings.TrimSpace(hc.URL))
		if perr != nil {
			return fmt.Errorf("健康检查 URL 非法:%v", perr)
		}
		scheme := u.Scheme
		if scheme == "" {
			scheme = "http"
		}
		probe := *hc
		probe.URL = scheme + "://" + ip + ":" + strconv.Itoa(inst.Port) + u.RequestURI()
		return s.runHealthCheck(ctx, srv.ID, &probe)
	}
	if hc != nil && hc.Type == HealthCheckCommand {
		return s.runHealthCheck(ctx, srv.ID, hc)
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

// containerIP 读容器首个网络 IP(多网络取第一个;预热探测足够)。
func (s *service) containerIP(ctx context.Context, serverID, container string) string {
	out, err := s.exec(ctx, serverID, []string{
		"docker", "inspect", "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}", container,
	})
	if err != nil || out == nil || out.ExitCode != 0 {
		return ""
	}
	fields := strings.Fields(out.Stdout)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
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

// firstString 返回首个非空白串。
func firstString(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
