// routes.go 是集群路由隔离策略:哪些通告网段允许自动进入节点路由表。
//
// 约定(跨环境网段重叠 = 生产事故,故默认收紧):
//   - 自动路由仅限**代码生成的各环境网关 /32**(跨环境转发数据面依赖,见
//     servicereg.GatewayProxyNetworks 与 meshroute);
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
//	自动项 = 本端网关 /32(gateways)∪ 路由表中已见网关 /32(seenCIDRs 里仅取 /32,
//	首连引导:远端网关经发现循环进入路由表后并入白名单,指纹变化自动重启生效);
//	手动项 = 管理员显式接受的网段(accepts)。
//
// 返回排序去重列表(空列表 = 拒绝全部通告,fail-closed)。
func RouteWhitelist(gateways, seenCIDRs, accepts []string) []string {
	set := make(map[string]struct{}, len(gateways)+len(accepts))
	for _, g := range gateways {
		if g != "" {
			set[g] = struct{}{}
		}
	}
	for _, c := range seenCIDRs {
		// 仅 /32:网关通告恒为 /32;自定义网段按约定不允许 /32(SaveSettings 拦截),
		// 故 /32 即网关,可安全自动接受。
		if c != "" && strings.HasSuffix(c, "/32") {
			set[c] = struct{}{}
		}
	}
	for _, a := range accepts {
		if a != "" {
			set[a] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}
