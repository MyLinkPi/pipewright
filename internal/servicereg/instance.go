package servicereg

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Instance 是服务的一个上游实例:网关同机上一个具名容器。http+container 服务的 upstream
// 由其实例组成(attached=1 渲染进 upstream 并接入共享网络;attached=0 摘除,仅留存配置)。
type Instance struct {
	ID        string
	ServiceID string
	Container string
	Port      int // 0 = 继承服务 UpstreamPort
	Attached  bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// DeployInstance 是 deploy 实例轮转(instance_rolling)消费的实例引用:服务 + 实例一体,
// 由 main.go 适配进 deploy.InstanceGateway(deploy 不 import 本包,晚绑防环)。
type DeployInstance struct {
	ServiceID   string
	ServiceName string // FQDN(展示/部署文案)
	InstanceID  string
	Container   string
	Port        int // 生效端口(实例端口或服务默认)
}

// EffectivePort 返回实例生效端口(实例端口 >0 优先,否则服务默认端口)。
func (i Instance) EffectivePort(svc *RegisteredService) int {
	if i.Port > 0 {
		return i.Port
	}
	return svc.UpstreamPort
}

// 领域错误(实例域)。
var (
	// ErrInstanceTaken 表示该服务下实例容器名已被占用((service_id, container) 唯一)。
	ErrInstanceTaken = errors.New("servicereg: instance container already in use")
	// ErrInvalidInstance 表示实例容器名/端口非法。
	ErrInvalidInstance = errors.New("servicereg: invalid instance")
	// ErrNotContainerService 表示目标服务不是 http+container 形态(实例仅适用于它)。
	ErrNotContainerService = errors.New("servicereg: instances only apply to http services with container upstream")
)

// ---------- Service 实现(实例管理) ----------

func (s *service) ListInstances(ctx context.Context, serviceID string) ([]Instance, error) {
	return s.store.listInstances(ctx, serviceID)
}

func (s *service) AddInstance(ctx context.Context, serviceID, container string, port int) (*Instance, error) {
	svc, err := s.store.getService(ctx, serviceID)
	if err != nil {
		return nil, err
	}
	container = strings.TrimSpace(container)
	if container == "" || len(container) > 128 || !dockerNameRe.MatchString(container) {
		return nil, ErrInvalidInstance
	}
	if port < 0 || port > 65535 {
		return nil, ErrInvalidInstance
	}
	if svc.Protocol != ProtocolHTTP || svc.UpstreamKind != UpstreamKindContainer {
		return nil, ErrNotContainerService
	}
	now := time.Now().UTC()
	inst := &Instance{
		ID: uuid.NewString(), ServiceID: serviceID, Container: container,
		Port: port, Attached: true, CreatedAt: now, UpdatedAt: now,
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

// SwapInstance 原子替换实例容器:实例行改名(old → new)+ 单次全量 apply/reload,
// upstream 成员在同一次 reload 中「old 出、new 进」(调用方须已把 new 容器起好并健康)。
// apply 失败 → 行回滚为 old(声明状态与部署状态一致)。
func (s *service) SwapInstance(ctx context.Context, serviceID, oldContainer, newContainer string) error {
	svc, err := s.store.getService(ctx, serviceID)
	if err != nil {
		return err
	}
	if svc.Protocol != ProtocolHTTP || svc.UpstreamKind != UpstreamKindContainer {
		return ErrNotContainerService
	}
	oldContainer = strings.TrimSpace(oldContainer)
	newContainer = strings.TrimSpace(newContainer)
	if newContainer == "" || len(newContainer) > 128 || !dockerNameRe.MatchString(newContainer) {
		return ErrInvalidInstance
	}
	inst, err := s.store.getInstanceByContainer(ctx, serviceID, oldContainer)
	if err != nil {
		return err
	}
	if err := s.store.updateInstanceContainer(ctx, inst.ID, newContainer); err != nil {
		return err
	}
	if err := s.applyBestEffort(ctx); err != nil {
		// 回滚行:upstream 未切,new 容器不属于本服务。
		_ = s.store.updateInstanceContainer(ctx, inst.ID, oldContainer)
		return err
	}
	return nil
}

// ResolveDeployInstances 供 deploy 实例轮转反查:按「实例容器名精确匹配」或「服务名匹配」
// (服务 abc 的实例惯例命名 abc-1/abc-2…)找到所属服务,返回其全部 attached 实例(生效端口)。
// 约束:serverID 必须是任一台网关主机(多网关机器均可联动);仅 http+container 服务参与
// (其余形态返回空,deploy 回退旧滚动)。
func (s *service) ResolveDeployInstances(ctx context.Context, serverID, container string) ([]DeployInstance, error) {
	container = strings.TrimSpace(container)
	if container == "" {
		return []DeployInstance{}, nil
	}
	st, err := s.store.getOrCreateSettings(ctx)
	if err != nil {
		return nil, err
	}
	if !st.HasServer(serverID) {
		return []DeployInstance{}, nil
	}
	services, err := s.store.listServices(ctx)
	if err != nil {
		return nil, err
	}
	instances, err := s.store.listAllInstances(ctx)
	if err != nil {
		return nil, err
	}

	// 匹配:先按实例容器名(任一服务的实例),再按服务名。
	var svc *RegisteredService
	for i := range services {
		c := &services[i]
		if c.Protocol != ProtocolHTTP || c.UpstreamKind != UpstreamKindContainer || !c.Enabled {
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
	if svc == nil {
		return []DeployInstance{}, nil
	}

	// FQDN 展示名。
	dom, derr := s.store.getDomain(ctx, svc.DomainID)
	name := svc.Name
	if derr == nil {
		name = svc.Name + "." + dom.BaseDomain
	}
	refs := make([]DeployInstance, 0)
	for _, inst := range instances {
		if inst.ServiceID != svc.ID || !inst.Attached {
			continue
		}
		refs = append(refs, DeployInstance{
			ServiceID: svc.ID, ServiceName: name,
			InstanceID: inst.ID, Container: inst.Container,
			Port: inst.EffectivePort(svc),
		})
	}
	return refs, nil
}
