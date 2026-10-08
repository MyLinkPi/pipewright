package servicereg

// instance.go 实现集群级服务实例模型:一个 Instance = 某服务在一台服务器上的一个端点
// (服务器地址:宿主端口)。网关对全部网关主机渲染**同一份**配置,upstream 成员 = 集群内
// 全部 attached 实例(本机没有部署的服务同样代理)。
//
//   - 容器实例(container 非空):由部署自动注册 —— 容器名 + 部署时范围探测分配的宿主端口
//     (docker -p host:container);升级走「扩容起新 → 预热 → 原子切换 → 排空 → 缩容停旧」。
//   - 非容器实例(container 空):同样由部署注册(进程监听端口即宿主端口)或人工添加;
//     升级走「摘除 → 原地升级 → 健康通过 → 挂回」,集群可用性由其余机器实例保证。
//
// 存量兼容:0068 之前的老行 server_id=''(网关本机共享网络模型)——渲染时跳过,
// 下次部署经 EnsureInstance 按(服务, 容器名)认领并补齐 server_id/host_port。

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Instance 是服务的一个集群端点:某台服务器上的一个进程/容器实例。
type Instance struct {
	ID        string
	ServiceID string
	// ServerID 是实例归属服务器(target servers 引用);'' = 历史遗留行(渲染跳过,待部署认领)。
	ServerID string
	// Container 是容器实例的容器名;非容器实例为空串。
	Container string
	// Port 是服务端口:容器实例 = 容器内应用端口;非容器实例 = 进程监听端口(与 HostPort 同义)。
	Port int
	// HostPort 是网关反代用的宿主端口:容器实例 = 部署时自动分配(-p host:container 的 host 侧);
	// 0 = 继承 Port。
	HostPort int
	// Attached 表示是否渲染进网关 upstream(摘除 = 升级窗口/故障机不回流量,行保留)。
	Attached  bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// DeployInstance 是 deploy 侧消费的实例引用:服务 + 实例一体,由 main.go 适配进
// deploy.InstanceGateway(deploy 不 import 本包,晚绑防环)。
type DeployInstance struct {
	ServiceID   string
	ServiceName string // FQDN(展示/部署文案)
	InstanceID  string
	ServerID    string // 实例归属服务器(= 部署目标机)
	Container   string // 容器名;非容器实例为空
	Port        int    // 生效服务端口(实例端口 >0 优先,否则服务默认)
	HostPort    int    // 生效宿主端口(网关反代/宿主健康检查用)
}

// EffectivePort 返回实例生效服务端口(实例端口 >0 优先,否则服务默认端口)。
func (i Instance) EffectivePort(svc *RegisteredService) int {
	if i.Port > 0 {
		return i.Port
	}
	return svc.UpstreamPort
}

// EffectiveHostPort 返回网关反代用的宿主端口(实例 HostPort >0 优先,否则回落服务端口)。
func (i Instance) EffectiveHostPort(svc *RegisteredService) int {
	if i.HostPort > 0 {
		return i.HostPort
	}
	return i.EffectivePort(svc)
}

// 领域错误(实例域)。
var (
	// ErrInstanceTaken 表示该服务下实例槽位已被占用((service_id, server_id, container) 唯一)。
	ErrInstanceTaken = errors.New("servicereg: instance slot already in use")
	// ErrInvalidInstance 表示实例参数非法(服务器/容器名/端口)。
	ErrInvalidInstance = errors.New("servicereg: invalid instance")
	// ErrNotHTTPService 表示目标服务不是 http 形态(实例池仅覆盖 http;tcp 保持单上游)。
	ErrNotHTTPService = errors.New("servicereg: instances only apply to http services")
)

// validateInstanceInput 校验实例入参(server 必填;容器实例容器名合法;端口范围合法;
// 服务必须 http)。port/hostPort 均可为 0 = 继承服务默认端口(渲染侧 EffectiveHostPort 回落)。
func (s *service) validateInstanceInput(svc *RegisteredService, serverID, container string, port, hostPort int) error {
	serverID = strings.TrimSpace(serverID)
	if serverID == "" || len(serverID) > 64 {
		return ErrInvalidInstance
	}
	container = strings.TrimSpace(container)
	if container != "" && (len(container) > 128 || !dockerNameRe.MatchString(container)) {
		return ErrInvalidInstance
	}
	if port < 0 || port > 65535 || hostPort < 0 || hostPort > 65535 {
		return ErrInvalidInstance
	}
	if svc.Protocol != ProtocolHTTP {
		return ErrNotHTTPService
	}
	return nil
}

// ---------- Service 实现(实例管理) ----------

func (s *service) ListInstances(ctx context.Context, serviceID string) ([]Instance, error) {
	return s.store.listInstances(ctx, serviceID)
}

// AddInstance 人工添加实例(默认 attached;触发 apply,失败回滚删除)。
// container 空 = 非容器实例(hostPort 即进程端口);非空 = 容器实例。
func (s *service) AddInstance(ctx context.Context, serviceID, serverID, container string, port, hostPort int) (*Instance, error) {
	svc, err := s.store.getService(ctx, serviceID)
	if err != nil {
		return nil, err
	}
	if err := s.validateInstanceInput(svc, serverID, container, port, hostPort); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	inst := &Instance{
		ID: uuid.NewString(), ServiceID: serviceID,
		ServerID: strings.TrimSpace(serverID), Container: strings.TrimSpace(container),
		Port: port, HostPort: hostPort, Attached: true, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.insertInstance(ctx, inst); err != nil {
		return nil, err
	}
	// 编排失败回滚删除(声明状态与部署状态一致);网关未配置时静默跳过。
	if err := s.applyBestEffort(ctx); err != nil {
		_ = s.store.deleteInstance(ctx, inst.ID)
		return nil, err
	}
	return inst, nil
}

// EnsureInstance 部署期幂等 upsert(一键注册的部署联动):按(服务, 服务器, 容器)定位 ——
// 精确命中 → 更新端口并置 attached;命中同(服务, 容器)的遗留行(server_id='')→ 认领
// (补 server_id + 端口);否则插入。返回最终实例;apply 失败时返回实例 + 错误(行保留,
// 下轮 apply 自愈 —— 部署已把进程拉起,不应因网关暂不可达回滚注册)。
func (s *service) EnsureInstance(ctx context.Context, serviceID, serverID, container string, port, hostPort int) (*Instance, error) {
	svc, err := s.store.getService(ctx, serviceID)
	if err != nil {
		return nil, err
	}
	if err := s.validateInstanceInput(svc, serverID, container, port, hostPort); err != nil {
		return nil, err
	}
	serverID = strings.TrimSpace(serverID)
	container = strings.TrimSpace(container)

	inst := s.store.findInstanceForUpsert(ctx, serviceID, serverID, container)
	now := time.Now().UTC()
	if inst == nil {
		inst = &Instance{
			ID: uuid.NewString(), ServiceID: serviceID, ServerID: serverID, Container: container,
			Port: port, HostPort: hostPort, Attached: true, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.store.insertInstance(ctx, inst); err != nil {
			return nil, err
		}
	} else if err := s.store.updateInstanceEndpoint(ctx, inst.ID, serverID, port, hostPort, true); err != nil {
		return nil, err
	}
	applyErr := s.applyBestEffort(ctx)
	inst, err = s.store.getInstance(ctx, inst.ID)
	if err != nil {
		return nil, err
	}
	return inst, applyErr
}

// EnsureInstanceDetached 部署期「预注册」(摘挂升级流程的第一步):确保实例行存在且**本轮
// 部署期间不进 upstream** —— 新行以 attached=false 落库;存量行(含遗留 '' 行认领)只更新
// 端口/归属,attached 原状保持(摘挂流程随后统一摘除,部署成功再挂回)。不触发 apply:
// detached 行本就不渲染,attached 存量行的端口变化由紧随的摘/挂 apply 承载。
func (s *service) EnsureInstanceDetached(ctx context.Context, serviceID, serverID, container string, port, hostPort int) (*Instance, error) {
	svc, err := s.store.getService(ctx, serviceID)
	if err != nil {
		return nil, err
	}
	if err := s.validateInstanceInput(svc, serverID, container, port, hostPort); err != nil {
		return nil, err
	}
	serverID = strings.TrimSpace(serverID)
	container = strings.TrimSpace(container)

	inst := s.store.findInstanceForUpsert(ctx, serviceID, serverID, container)
	now := time.Now().UTC()
	if inst == nil {
		inst = &Instance{
			ID: uuid.NewString(), ServiceID: serviceID, ServerID: serverID, Container: container,
			Port: port, HostPort: hostPort, Attached: false, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.store.insertInstance(ctx, inst); err != nil {
			return nil, err
		}
		return inst, nil
	}
	if err := s.store.updateInstanceEndpoint(ctx, inst.ID, serverID, port, hostPort, inst.Attached); err != nil {
		return nil, err
	}
	return s.store.getInstance(ctx, inst.ID)
}

// PruneInstancesNotIn 同步清理:删除该服务下 server_id 不在 serverIDs 内的实例(含遗留 ''
// 行),返回删除数;触发 apply。部署全部目标成功后调用,使实例表与实际部署拓扑一致。
func (s *service) PruneInstancesNotIn(ctx context.Context, serviceID string, serverIDs []string) (int, error) {
	n, err := s.store.deleteInstancesNotIn(ctx, serviceID, serverIDs)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	if err := s.applyBestEffort(ctx); err != nil {
		return int(n), err
	}
	return int(n), nil
}

func (s *service) RemoveInstance(ctx context.Context, instanceID string) error {
	if err := s.store.deleteInstance(ctx, instanceID); err != nil {
		return err
	}
	return s.applyBestEffort(ctx)
}

func (s *service) SetInstanceAttached(ctx context.Context, instanceID string, attached bool) error {
	if err := s.store.setInstanceAttached(ctx, instanceID, attached); err != nil {
		return err
	}
	return s.applyBestEffort(ctx)
}

// SwapInstance 原子替换实例容器(容器「扩缩容轮转」的切流量原语):实例行改名
// old → new 并同步更新宿主端口,单次全量 apply/reload —— upstream 成员在同一次 reload 中
// 「old 出、new 进」。调用方须已把 new 容器起好并健康。hostPort<=0 = 保持原 host_port
// (未发布宿主端口的存量实例)。apply 失败 → 行整体回滚。
func (s *service) SwapInstance(ctx context.Context, serviceID, serverID, oldContainer, newContainer string, hostPort int) error {
	svc, err := s.store.getService(ctx, serviceID)
	if err != nil {
		return err
	}
	if svc.Protocol != ProtocolHTTP {
		return ErrNotHTTPService
	}
	oldContainer = strings.TrimSpace(oldContainer)
	newContainer = strings.TrimSpace(newContainer)
	if newContainer == "" || len(newContainer) > 128 || !dockerNameRe.MatchString(newContainer) {
		return ErrInvalidInstance
	}
	if hostPort < 0 || hostPort > 65535 {
		return ErrInvalidInstance
	}
	inst := s.store.findInstanceForUpsert(ctx, serviceID, strings.TrimSpace(serverID), oldContainer)
	if inst == nil {
		return ErrNotFound
	}
	if err := s.store.updateInstanceSwap(ctx, inst.ID, newContainer, hostPort); err != nil {
		return err
	}
	if err := s.applyBestEffort(ctx); err != nil {
		// 回滚行:upstream 未切,new 容器不属于本服务。
		_ = s.store.updateInstanceSwap(ctx, inst.ID, oldContainer, inst.HostPort)
		return err
	}
	return nil
}

// ResolveDeployInstances 供 deploy 反查:定位服务后返回**该 serverID 上**的 attached 实例
// (滚动升级只动本机,绝不摘其它机器的实例)。定位顺序:
//
//  1. serviceRef 精确匹配服务 ID(部署节点「一键生成」的 regServiceId 绑定);
//  2. container 匹配任一 http 服务的实例容器名;
//  3. container 匹配服务名(服务 abc 的实例惯例命名 abc-1/abc-2…,存量兼容)。
//
// 返回实例含遗留行(server_id='' —— 本机部署先行认领,旧模型平滑迁移)。非 http /
// 未启用服务不参与。serverID 无需是网关主机(集群模型:任何部署目标机都可承载实例)。
func (s *service) ResolveDeployInstances(ctx context.Context, serverID, serviceRef, container string) ([]DeployInstance, error) {
	serverID = strings.TrimSpace(serverID)
	container = strings.TrimSpace(container)
	serviceRef = strings.TrimSpace(serviceRef)

	var svc *RegisteredService
	if serviceRef != "" {
		if v, err := s.store.getService(ctx, serviceRef); err == nil {
			svc = v
		}
	}
	if svc == nil && container != "" {
		services, err := s.store.listServices(ctx)
		if err != nil {
			return nil, err
		}
		instances, err := s.store.listAllInstances(ctx)
		if err != nil {
			return nil, err
		}
		for i := range services {
			c := &services[i]
			if c.Protocol != ProtocolHTTP || !c.Enabled {
				continue
			}
			if c.Name == container {
				svc = c
				break
			}
			for _, inst := range instances {
				if inst.ServiceID == c.ID && inst.Container == container {
					svc = c
					break
				}
			}
			if svc != nil {
				break
			}
		}
	}
	if svc == nil || !svc.Enabled || svc.Protocol != ProtocolHTTP {
		return []DeployInstance{}, nil
	}

	instances, err := s.store.listInstances(ctx, svc.ID)
	if err != nil {
		return nil, err
	}
	// FQDN 展示名。
	dom, derr := s.store.getDomain(ctx, svc.DomainID)
	name := svc.Name
	if derr == nil {
		name = svc.Name + "." + dom.BaseDomain
	}
	refs := make([]DeployInstance, 0)
	for _, inst := range instances {
		// 本机实例 + 遗留行(server_id='' 视为本机候选,由本次部署认领)。
		if !inst.Attached || (inst.ServerID != serverID && inst.ServerID != "") {
			continue
		}
		refs = append(refs, DeployInstance{
			ServiceID: svc.ID, ServiceName: name,
			InstanceID: inst.ID, ServerID: inst.ServerID, Container: inst.Container,
			Port: inst.EffectivePort(svc), HostPort: inst.EffectiveHostPort(svc),
		})
	}
	return refs, nil
}
