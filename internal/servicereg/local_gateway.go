package servicereg

// local_gateway.go 是「本机(控制节点)作网关主机」的共存层(specs/local-node-roles.md 7.2/7.3):
//   - 角色门禁:ServerIDs 含 __local__ 须 gateway_enabled=true(main 注入 localnode 判定)。
//   - 端口共存:平台 HTTPS 占宿主 80/443 时,本机网关容器改绑 127.0.0.1 回环端口,
//     由 platformhttps 渲染的「入口 vhost」反代进来(证书就绪的基域逐个 server 块)。
//   - 入口 vhost 联动:设置变更 / apply 后重算 GatewayEntrySpec 并发布;本机移出即摘除。
//   - 混合网关守卫:host.docker.internal 只在「网关容器跑在本机」时成立,实例池 upstream
//     地址会在全部网关主机渲染同一份配置 —— 本机承载实例 + 远端网关主机并存 → 渲染期显式
//     拒绝(唯一能覆盖「先部署实例、后把本机加进网关」时序的拦截点)。
//
// 依赖方向:本包经 main 晚绑注入 platformhttps 发布面与 certmgmt 证书源,不反向依赖
// (platformhttps 不 import servicereg;certmgmt 经接口适配)。

import (
	"context"
	"fmt"

	"github.com/huangchengsir/pipewright/internal/platformhttps"
	"github.com/huangchengsir/pipewright/internal/target"
)

// GatewayEntryPublisher 是「本机网关入口 vhost」发布面(platformhttps 实现,main 晚绑注入;
// 防包间反向依赖)。PEM 由调用方解密传入,实现方只落盘 + nginx -t + reload。
type GatewayEntryPublisher interface {
	// ApplyGateway 全量收敛入口 vhost(基域列表 + 每域证书 + 回环端口);幂等。
	ApplyGateway(ctx context.Context, spec platformhttps.GatewayEntrySpec) error
	// RemoveGateway 摘除入口 vhost 与其证书(幂等;未发布过 = 成功)。
	RemoveGateway(ctx context.Context) error
}

// GatewayEntrySource 抽象「按基域解析入口 vhost 证书 PEM」(main 适配 certmgmt;PEM 仅
// 进程内传递,绝不日志/落库)。ok=false = 该基域暂无匹配证书(证书就绪门未过,跳过渲染)。
type GatewayEntrySource interface {
	GatewayEntryCert(ctx context.Context, baseDomain string) (certID, fullchainPEM, keyPEM string, ok bool, err error)
}

// LocalGatewayHooks 聚合本机网关共存的晚绑依赖(main 装配;nil 字段各自降级)。
type LocalGatewayHooks struct {
	// GatewayEnabled 报告本机网关角色是否启用(localnode);nil = 未启用(哨兵必拒)。
	GatewayEnabled func() bool
	// Coexistence 报告平台 HTTPS 是否已启用(共存模式判定);nil = 恒直连模式。
	Coexistence func() bool
	// Publisher 是入口 vhost 发布面;nil = 不发布(共存模式判定随之失效)。
	Publisher GatewayEntryPublisher
	// Source 按基域解析入口证书;nil = 仅回落本网关基域的自上传证书。
	Source GatewayEntrySource
}

// SetLocalGatewayHooks 注入本机网关共存依赖(main.go 晚绑;specs 7.3)。
func (s *service) SetLocalGatewayHooks(h LocalGatewayHooks) { s.localHooks = &h }

// localGatewayEnabled 报告本机网关角色是否启用(钩子未注入 = 否)。
func (s *service) localGatewayEnabled() bool {
	return s.localHooks != nil && s.localHooks.GatewayEnabled != nil && s.localHooks.GatewayEnabled()
}

// localCoexistence 报告本机网关是否处于共存模式(平台 HTTPS 已启用 + 发布面已装配)。
func (s *service) localCoexistence() bool {
	return s.localHooks != nil && s.localHooks.Coexistence != nil && s.localHooks.Coexistence() &&
		s.localHooks.Publisher != nil
}

// guardMixedGateway 渲染期混合网关守卫(specs 7.2):本机承载的 attached 实例要求
// ServerIDs 全为本机(唯一网关主机)。实例 upstream 地址在全部网关主机渲染同一份配置,
// 远端网关容器里的 host.docker.internal 指向远端宿主 → 静默错路由,必须显式拒绝。
// 这是唯一能覆盖「先部署实例、后把本机加进网关 ServerIDs」时序的拦截点(保存期两份配置
// 独立保存,挡不住运行期漂移);普通部署到本机(未声明实例注册)不经此路径,不受此限。
func guardMixedGateway(st *Settings, instances []Instance) error {
	var localHosted string // 本机承载的实例 serviceID(空 = 无本机承载实例)
	for _, inst := range instances {
		if inst.Attached && inst.ServerID == target.LocalHostID {
			localHosted = inst.ServiceID
			break
		}
	}
	if localHosted == "" {
		return nil
	}
	// 允许的唯一形态:本机是网关主机且为唯一一台(ServerIDs 非空且全为本机)—— 此时入口
	// vhost 与网关容器都在本机,host.docker.internal 语义成立。本机不在网关列表 / 混合网关
	// (本机 + 远端 / 列表为空)→ 拒绝:远端网关容器里的 host.docker.internal 指向远端宿主,
	// 静默错路由。
	onlyLocal := len(st.ServerIDs) > 0
	for _, sid := range st.ServerIDs {
		if sid != target.LocalHostID {
			onlyLocal = false
			break
		}
	}
	if onlyLocal {
		return nil
	}
	return fmt.Errorf("本机承载的实例 %s 不能与远端网关主机共存(含网关主机列表为空):upstream 地址 host.docker.internal 只在网关容器跑在本机时成立;请将网关主机设为本机单独一台,或将该服务部署到远端机器", localHosted)
}

// syncGatewayEntry 收敛本机网关入口 vhost(联动点:UpdateSettings / applyFull):
//   - 本机是网关主机 + 共存模式开启:逐基域解析证书(就绪门未过的域跳过渲染)→ 发布;
//   - 本机移出 / 无任何就绪证书 / 钩子不全:摘除(幂等)。
// 证书来源:先 certmgmt(Source 注入),未命中回落本网关基域的自上传证书(与网关容器
// 同一份,复用 store 解密);两处皆无 → 该域跳过。
func (s *service) syncGatewayEntry(ctx context.Context, st *Settings) error {
	if s.localHooks == nil || s.localHooks.Publisher == nil {
		return nil
	}
	if !st.HasServer(target.LocalHostID) || !s.localGatewayEnabled() || !s.localCoexistence() {
		return s.localHooks.Publisher.RemoveGateway(ctx)
	}
	// 共存模式下平台 nginx 占用宿主 80/443,网关容器必须回环绑定独立端口;回环端口未配置
	// (0)会让容器渲染 127.0.0.1:0 —— 启动即失败,必须在渲染前显式拒绝而非下发坏配置。
	if st.LocalHTTPPort < 1 || st.LocalHTTPPort > 65535 ||
		st.LocalHTTPSPort < 1 || st.LocalHTTPSPort > 65535 {
		return fmt.Errorf("网关入口 vhost:共存模式要求配置合法回环端口(local_http_port/local_https_port),当前 %d/%d", st.LocalHTTPPort, st.LocalHTTPSPort)
	}
	domains, err := s.store.listDomains(ctx)
	if err != nil {
		return fmt.Errorf("网关入口 vhost:读取基域失败:%w", err)
	}
	spec := platformhttps.GatewayEntrySpec{
		LoopbackHTTPPort:  st.LocalHTTPPort,
		LoopbackHTTPSPort: st.LocalHTTPSPort,
	}
	for _, d := range domains {
		certID, fc, key, ok, err := s.gatewayEntryCert(ctx, d)
		if err != nil {
			return fmt.Errorf("网关入口 vhost:解析基域 %s 证书失败:%w", d.BaseDomain, err)
		}
		if !ok {
			// 证书就绪门未过:该域跳过渲染(UI 明示先去证书管理签发 DNS-01 通配符,
			// 或给基域上传证书);全部跳过时走下方摘除分支。
			continue
		}
		spec.Certs = append(spec.Certs, platformhttps.GatewayEntryCert{
			BaseDomain: d.BaseDomain, CertID: certID, FullchainPEM: fc, PrivkeyPEM: key,
		})
	}
	if len(spec.Certs) == 0 {
		return s.localHooks.Publisher.RemoveGateway(ctx)
	}
	return s.localHooks.Publisher.ApplyGateway(ctx, spec)
}

// gatewayEntryCert 解析基域入口证书:certmgmt(Source)优先,未命中回落本网关基域的
// 自上传证书(service_reg_domains;与网关容器在用的同一份,经 vault 解密)。ok=false =
// 两处皆无(就绪门未过)。
func (s *service) gatewayEntryCert(ctx context.Context, d Domain) (certID, fullchain, key string, ok bool, err error) {
	if s.localHooks != nil && s.localHooks.Source != nil {
		cid, fc, k, ok, gerr := s.localHooks.Source.GatewayEntryCert(ctx, d.BaseDomain)
		if gerr != nil {
			return "", "", "", false, gerr
		}
		if ok {
			return cid, fc, k, true, nil
		}
	}
	if !d.HasCert {
		return "", "", "", false, nil
	}
	sealedCert, sealedKey, have, gerr := s.store.getCertSealed(ctx, d.ID)
	if gerr != nil || !have {
		return "", "", "", false, nil
	}
	fcPlain, uerr := s.vault.OpenSecret(sealedCert)
	if uerr != nil {
		return "", "", "", false, nil // vault 不可用:按就绪门未过处理,不阻塞其余基域
	}
	keyPlain, uerr := s.vault.OpenSecret(sealedKey)
	if uerr != nil {
		return "", "", "", false, nil
	}
	return d.ID, string(fcPlain), string(keyPlain), true, nil
}
