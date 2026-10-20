// routes.go 是集群路由隔离策略:哪些通告网段允许自动进入节点路由表。
//
// 约定(跨环境网段重叠 = 生产事故,故默认收紧):
//   - 自动路由仅限**对端环境**代码生成的网关 /32(跨环境转发数据面依赖,见
//     servicereg.GatewayProxyNetworks 与 meshroute);
//   - **本端通告的一切网段(含本端网关 /32)绝不进本机路由**:它们经局域网本就可达,
//     进白名单会被装成 tun 路由,把发往本端网关的流量(SSH 对账/转发)吸进隧道黑洞,
//     直接断联(v1.6.4 事故);
//   - 管理员自定义通告网段(proxy_networks_custom)绝不自动路由 —— 任何环境都不;
//   - 需要跨环境访问某网段 = 在本节点 accept_networks_custom 显式确认(手动项);
//   - pwclient 客户端同构:留空不路由任何通告,访问哪个网段显式加入列表。
package mesh

import (
	"sort"
	"strings"
)

// RouteWhitelist 计算平台节点的 --manual-routes 白名单:
//
//	自动项 = 路由表已见的**对端**网关 /32(seenCIDRs 仅取 /32 且剔除本端通告 ownCIDRs;
//	首连引导:远端网关经发现进入路由表后并入,指纹变化自动重启生效);
//	手动项 = 管理员显式接受的网段(accepts,同样剔除本端通告)。
//
// ownCIDRs(本端通告 = 自定义网段 ∪ 本端网关 /32)只用于 --proxy-networks 通告;
// 误入白名单会把本端可达网段装成 tun 路由(v1.6.4 事故:本端网关 SSH 断联)。
//
// 返回排序去重列表(空列表 = 拒绝全部通告,fail-closed)。
func RouteWhitelist(ownCIDRs, seenCIDRs, accepts []string) []string {
	own := make(map[string]struct{}, len(ownCIDRs))
	for _, c := range ownCIDRs {
		if c != "" {
			own[c] = struct{}{}
		}
	}
	set := make(map[string]struct{}, len(accepts))
	add := func(c string) {
		if c == "" {
			return
		}
		if _, isOwn := own[c]; isOwn {
			return
		}
		set[c] = struct{}{}
	}
	for _, c := range seenCIDRs {
		// 仅 /32:网关通告恒为 /32;自定义网段按约定不允许 /32(SaveSettings 拦截),
		// 故 /32 即对端网关,可安全自动接受。
		if c != "" && strings.HasSuffix(c, "/32") {
			add(c)
		}
	}
	for _, a := range accepts {
		add(a)
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}
