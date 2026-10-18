// meshroute.go 实现跨环境转发的数据面路由编排:在本端每台网关机上下发/撤除指向对端
// 网关 IP 的 /32 主机路由,下一跳 = 本机(pipewright 宿主)在网关机可达子网上的地址;
// 本机再经 EasyTier 子网代理(--proxy-network,见 mesh 运行时)把流量送进 mesh。
//
//	网关机 --[ip route replace <对端网关IP>/32 via <本机LAN IP>]--> 本机(ip_forward=1)
//	  --EasyTier 子网代理--> 对端 pipewright 宿主 --> 对端网关机(回程对称)
//
// 纪律:
//   - 全部命令经 target.Exec 以 array 形式执行(不拼 shell);网关机 SSH 账号需 root 或
//     免密 sudo / 已配 sudo 凭据(ip route 需特权);
//   - 托管账本(settings.forward_routes,server_id → [IP])只记本平台下发的路由,
//     撤除仅针对账本目标 —— 绝不触碰管理员手工路由;
//   - 幂等对账:期望集(启用 forward 服务引用对端的网关 IP)逐台校验在位情况,缺失补下
//     (ip route replace 自愈重启/漂移),账本内多余项撤除;全部经由 30s 发现循环节拍 +
//     每次 apply 触发。
package servicereg

import (
	"context"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/huangchengsir/pipewright/internal/netforward"
)

// ReconcileForwardRoutes 实现接口:对账全部网关机上的跨环境 /32 路由(幂等;错误记入
// 状态快照,不向调用方抛出 —— 发现循环按节拍自愈)。
func (s *service) ReconcileForwardRoutes(ctx context.Context) {
	st, err := s.store.getOrCreateSettings(ctx)
	if err != nil {
		return
	}
	services, err := s.store.listServices(ctx)
	if err != nil {
		return
	}
	ledger, err := s.store.getForwardRoutes(ctx)
	if err != nil {
		ledger = map[string][]string{}
	}
	// 期望集:启用 forward 服务可解析出的对端网关 IP(buildForwardPlans 已做冲突过滤与
	// 陈旧宽限;目录全不可达 → 期望空 → 账本内路由撤除)。
	var enabled []RegisteredService
	for _, svc := range services {
		if svc.Enabled {
			enabled = append(enabled, svc)
		}
	}
	plans, _ := s.buildForwardPlans(ctx, enabled)
	expected := map[string]struct{}{}
	for _, p := range plans {
		for _, ip := range p.Members {
			expected[ip] = struct{}{}
		}
	}

	// 本机 ip_forward 保活:存在期望路由或账本非空才动(未启用转发的实例零侵入)。
	if len(expected) > 0 || hasLedgerRoutes(ledger) {
		netforward.Ensure()
	}

	var status []GatewayRouteStatus
	newLedger := map[string][]string{}
	for _, sid := range st.ServerIDs {
		routes, _ := s.reconcileServerRoutes(ctx, sid, expected, ledger[sid])
		status = append(status, routes...)
		newLedger[sid] = ledgerRoutes(routes)
	}
	_ = s.store.setForwardRoutes(ctx, newLedger)
	s.fwd.mu.Lock()
	s.fwd.routeStatus = status
	s.fwd.mu.Unlock()
}

// reconcileServerRoutes 对账单台网关机:返回逐路由结果(present/applied/removed/missing)。
// 账本(managed)为空且期望为空时零 SSH(未启用转发的稳态)。
func (s *service) reconcileServerRoutes(ctx context.Context, serverID string, expected map[string]struct{}, managed []string) ([]GatewayRouteStatus, error) {
	if len(expected) == 0 && len(managed) == 0 {
		return nil, nil
	}
	// 本机对网关机子网的源地址(下一跳):UDP 拨号取路由选源,不实际发包。
	gwIP := ""
	if s.serverAddr != nil {
		if ip, err := s.serverAddr(ctx, serverID); err == nil {
			gwIP = ip
		}
	}
	nextHop := localSourceIP(gwIP)
	var out []GatewayRouteStatus
	if nextHop == "" {
		for ip := range expected {
			out = append(out, GatewayRouteStatus{ServerID: serverID, Dest: ip, State: "missing", Error: "无法确定本机到网关机的源地址(直连可达?)"})
		}
		sortRoutes(out)
		return out, ErrNoGateway
	}
	// 实际在位:ip route show 中下一跳为本机的目标。
	actual := map[string]bool{}
	if res, err := s.tg.Exec(ctx, serverID, []string{"ip", "route", "show"}); err == nil && res.ExitCode == 0 {
		actual = parseRoutesVia(res.Stdout, nextHop)
	}
	managedSet := map[string]struct{}{}
	for _, ip := range managed {
		managedSet[ip] = struct{}{}
	}
	// 期望增/补(缺失即 replace —— 幂等自愈重启丢失)。
	for ip := range expected {
		row := GatewayRouteStatus{ServerID: serverID, Dest: ip}
		switch {
		case actual[ip]:
			row.State = "present"
		default:
			res, err := s.tg.Exec(ctx, serverID, []string{"ip", "route", "replace", ip + "/32", "via", nextHop})
			switch {
			case err != nil:
				row.State, row.Error = "missing", err.Error()
			case res.ExitCode != 0:
				row.State, row.Error = "missing", strings.TrimSpace(firstNonEmpty(res.Stderr, res.Stdout))
			default:
				row.State = "applied"
			}
		}
		out = append(out, row)
	}
	// 账本撤除(期望已不含的目标;「路由不存在」视为已撤除,不报错)。
	for _, ip := range managed {
		if _, want := expected[ip]; want {
			continue
		}
		row := GatewayRouteStatus{ServerID: serverID, Dest: ip}
		res, err := s.tg.Exec(ctx, serverID, []string{"ip", "route", "del", ip + "/32"})
		switch {
		case err != nil:
			row.State, row.Error = "removed", err.Error()
		case res.ExitCode != 0 && !strings.Contains(strings.ToLower(firstNonEmpty(res.Stderr, res.Stdout)), "no such"):
			row.State, row.Error = "removed", strings.TrimSpace(firstNonEmpty(res.Stderr, res.Stdout))
		default:
			row.State = "removed"
		}
		out = append(out, row)
	}
	sortRoutes(out)
	return out, nil
}

// localSourceIP 返回本机访问 target 时选用的源 IP(UDP 拨号取路由,不发包;失败 → "")。
func localSourceIP(target string) string {
	if target == "" {
		return ""
	}
	conn, err := net.DialTimeout("udp", net.JoinHostPort(target, "9"), 2*time.Second)
	if err != nil {
		return ""
	}
	defer func() { _ = conn.Close() }()
	if a, ok := conn.LocalAddr().(*net.UDPAddr); ok && a.IP != nil {
		return a.IP.String()
	}
	return ""
}

// parseRoutesVia 解析 `ip route show` 输出,返回「下一跳为 via 的目标 IP」集合
// (主机路由显示为裸 IP,无 /32 后缀)。
func parseRoutesVia(out, via string) map[string]bool {
	dests := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && f[1] == "via" && f[2] == via {
			dests[f[0]] = true
		}
	}
	return dests
}

// hasLedgerRoutes 报告账本是否记有任何路由。
func hasLedgerRoutes(ledger map[string][]string) bool {
	for _, v := range ledger {
		if len(v) > 0 {
			return true
		}
	}
	return false
}

// ledgerRoutes 从对账结果提取应记账的目标(applied/present/removed 成功项 —— removed
// 成功即从账本消失;missing 失败项保留在账本,供下轮重试撤除)。
func ledgerRoutes(rows []GatewayRouteStatus) []string {
	var out []string
	for _, r := range rows {
		if r.State == "applied" || r.State == "present" {
			out = append(out, r.Dest)
		}
	}
	sort.Strings(out)
	return out
}

func sortRoutes(rows []GatewayRouteStatus) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Dest != rows[j].Dest {
			return rows[i].Dest < rows[j].Dest
		}
		return rows[i].State < rows[j].State
	})
}

// forwardStatusSnapshot 汇聚转发数据面状态(网关页展示;ForwardStatus.Routes 含每台网关机
// 逐路由对账结果,Conflicts 为 IP 冲突告警)。
func (s *service) forwardStatusSnapshot() ForwardStatus {
	s.fwd.mu.Lock()
	routes := append([]GatewayRouteStatus(nil), s.fwd.routeStatus...)
	conflicts := append([]string(nil), s.fwd.conflicts...)
	s.fwd.mu.Unlock()
	ipf := netforward.LastStatus()
	detail := ""
	if !ipf.Enabled || !ipf.Persisted || ipf.LastErr != "" {
		switch {
		case ipf.LastErr != "":
			detail = ipf.LastErr
		case !ipf.Enabled:
			detail = "ip_forward 未开启"
		default:
			detail = "ip_forward 未持久化(重启后可能失效)"
		}
	}
	return ForwardStatus{IPForwardOK: ipf.Enabled, IPForwardDetail: detail, Routes: routes, Conflicts: conflicts}
}
