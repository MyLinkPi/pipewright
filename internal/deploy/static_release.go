package deploy

// static_release.go 实现「网关静态发布」(deploy_static 节点):把 dist 产物发布到全部网关
// 主机的静态发布目录(<StaticRoot>/<服务ID>,由注册绑定解析),网关 nginx 容器经 bind mount
// 只读托管 —— 纯前端(前后端分离的静态侧)也能走服务注册一体化发布。
//
// 与产物 release 路径的关系:复用同一套「releases/<runId> + current 软链原子切换 + 失败回滚」
// 零停机机制(nginx 按请求读盘,软链切换即生效,无需 reload);差异仅三点:
//   - 目标 = 全部网关主机(节点无需也不应选择机器;多台网关自动全量覆盖);
//   - 跳过 restartCommand(网关容器无需感知文件更替);
//   - 健康门控默认退化为 current/index.html 存在性检查(静态发布无监听端口可探)。
// 静态服务无实例语义:不做实例注册/摘挂/注册表清理(内容即状态)。

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/target"
)

var (
	// ErrStaticNoBinding 表示静态发布节点缺少服务注册绑定(regServiceId / gatewayService)。
	ErrStaticNoBinding = errors.New("deploy: 静态发布节点未绑定静态服务,请先在节点的服务注册区块一键生成并绑定")
	// ErrStaticGatewayUnavailable 表示未装配服务注册网关,或网关适配器不支持静态发布解析。
	ErrStaticGatewayUnavailable = errors.New("deploy: 静态发布需要装配服务注册网关(且绑定服务须为静态托管形态)")
)

// StaticDeploySpec 是静态发布的目标解析结果(InstanceGateway 适配器返回;deploy 不感知
// servicereg 类型)。
type StaticDeploySpec struct {
	ServiceID string
	FQDN      string // 展示用(不含 path)
	Path      string // 展示用('' = 根路径)
	// ServerIDs 是全部网关主机(静态内容必须落到每一台网关,由各台网关的 nginx 本地托管)。
	ServerIDs []string
	// BaseDir 是各网关主机上的发布根目录(releaseBase:releases/<runId> + current 软链)。
	BaseDir string
}

// StaticDeployResolver 是 InstanceGateway 的可选扩展(经类型断言获取,不破坏既有实现/测试桩):
// 解析静态发布目标 —— 绑定服务须为 static 形态,返回发布目录与全部网关主机。
type StaticDeployResolver interface {
	ResolveStaticDeploy(ctx context.Context, serviceRef string) (StaticDeploySpec, error)
}

// deployStaticForStage 是 deploy_static 节点的执行入口(build 层 runDeployJob 以
// cfg["staticGateway"]="1" 标记路由至此):目标/目录由注册绑定决定,不走 selector。
func (s *service) deployStaticForStage(ctx context.Context, runID string, cfg map[string]string) ([]TargetResult, error) {
	ref := regServiceRef(cfg)
	if ref == "" {
		return nil, ErrStaticNoBinding
	}
	resolver, ok := s.instanceGateway.(StaticDeployResolver)
	if !ok || resolver == nil {
		return nil, ErrStaticGatewayUnavailable
	}
	spec, err := resolver.ResolveStaticDeploy(ctx, ref)
	if err != nil {
		return nil, err
	}

	// 目标 = 全部网关主机(逐台解析;任一不存在 → 整次拒绝:静态内容必须每台网关就位,
	// 静默跳过会造成多网关内容不一致)。
	servers := make([]*target.Server, 0, len(spec.ServerIDs))
	for _, sid := range spec.ServerIDs {
		srv, gerr := s.targets.Get(ctx, sid)
		if gerr != nil {
			if errors.Is(gerr, target.ErrNotFound) {
				return nil, ErrServerNotFound
			}
			return nil, gerr
		}
		servers = append(servers, srv)
	}

	// 恢复运行(按节点恢复)增量语义:与产物路径一致,已成功机器跳过重发布;持久化逐目标 upsert。
	persist := stagePersistFunc(s.runs.SaveDeployTargets)
	skipSucceeded, err := s.resumeSucceededServers(ctx, runID)
	if err != nil {
		return nil, err
	}
	if len(skipSucceeded) > 0 {
		filtered := make([]*target.Server, 0, len(servers))
		skippedNames := []string(nil)
		for _, srv := range servers {
			if skipSucceeded[srv.ID] {
				skippedNames = append(skippedNames, serverDisplayName(srv))
				continue
			}
			filtered = append(filtered, srv)
		}
		if len(filtered) == 0 {
			return skippedSuccessResults(servers, skipSucceeded), nil
		}
		servers = filtered
		persist = stagePersistFunc(s.runs.UpsertDeployTargets)
		if len(skippedNames) > 0 {
			cmdLogFrom(ctx)(cmdStreamStdout, "", fmt.Sprintf("· 已成功跳过重发布(%d 台网关):%s", len(skippedNames), strings.Join(skippedNames, ", ")))
		}
	}

	// 目标可见性:发布到哪个路由、覆盖哪几台网关,动手前明示到步骤日志。
	route := spec.FQDN + spec.Path
	if spec.Path == "" {
		route = spec.FQDN + "/"
	}
	cmdLogFrom(ctx)(cmdStreamStdout, "", fmt.Sprintf("→ 网关静态发布 %s(%d 台网关):%s", route, len(servers), strings.Join(serverDisplayNames(servers), ", ")))

	// 产物:与产物路径同一挑选逻辑;静态发布仅支持 dist(前端构建产物)。
	arts, err := s.runs.ListArtifacts(ctx, runID)
	if err != nil {
		return nil, err
	}
	var artifact *run.Artifact
	if sourceJob := strings.TrimSpace(cfg["artifactJob"]); sourceJob != "" {
		artifact, err = pickStageArtifactExplicit(arts, sourceJob, strings.TrimSpace(cfg["artifactName"]))
		if err != nil {
			return nil, err
		}
	} else {
		// 旧配置兜底:按类型偏好挑选。弃用路径 —— 仅存量流水线会走到。
		artifact = pickStageArtifact(arts, strings.TrimSpace(cfg["artifactType"]))
		if artifact == nil {
			return nil, ErrArtifactNotFound
		}
	}
	if artifact.Type != run.ArtifactDist {
		return nil, fmt.Errorf("静态发布仅支持 dist 产物,选中的产物类型为 %s(请绑定前端构建节点)", artifact.Type)
	}

	// 发布配置:releaseBase = 静态服务发布目录;跳过 restartCommand(nginx 按请求读盘,
	// 软链切换即生效)。复制 cfg,不污染调用方(流水线节点配置需可重复执行)。
	scfg := make(map[string]string, len(cfg)+1)
	for k, v := range cfg {
		scfg[k] = v
	}
	scfg["releaseBase"] = spec.BaseDir
	scfg["restartCommand"] = ""

	// 健康门控:显式 healthCommand / healthUrl 照用;port 型(无端口可探)与 exec 型(引用
	// 业务容器,静态发布不存在)退化为默认门控 —— current/index.html 存在性(dist 必含
	// index.html,缺失即产物错误,切换后立即回滚)。
	hsp := healthCheckSpecFromCfg(cfg, "")
	if hsp == nil || hsp.kind == specPort || hsp.kind == specExec {
		hsp = &healthSpec{
			kind:           specCommand,
			command:        []string{"test", "-f", path.Join(spec.BaseDir, "current", "index.html")},
			timeoutSeconds: 5,
		}
	}

	// 复用产物 release 滚动路径(deployOne 的 dist 分支):对静态绑定,摘挂包装天然空转
	// (静态服务反查不到实例),实际执行 = deployReleaseOne(发布 + 软链切换 + 门控 + 回滚)。
	results := s.deployRolling(ctx, servers, *artifact, scfg, hsp)

	// 持久化每机结果(不置 run 终态,dag 调度器控制);静态服务无实例,不做注册表清理。
	dts := make([]run.DeployTarget, 0, len(results))
	for _, r := range results {
		dts = append(dts, run.DeployTarget{
			RunID: runID, ServerID: r.ServerID, ServerName: r.ServerName,
			Status: r.Status, Message: r.Message, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
		})
	}
	if err := persist(ctx, runID, dts); err != nil {
		return nil, err
	}
	return results, nil
}
