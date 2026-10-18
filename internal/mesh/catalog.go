// catalog.go 定义跨环境服务转发的 wire 契约(服务目录):
//   - 提供方(被转发端):GET /api/mesh/servicereg/catalog(签名 + forward ACL,默认拒绝)
//     返回本端网关主机清单(已解析 IP,供对端网关机配 /32 路由)与可被转发的已启用服务;
//   - 消费方(转发端):servicereg 渲染 forward 服务时拉取授权对端目录,把对端网关
//     (IP:端口)渲染为本端 nginx 的 upstream 成员 / 流代理目标。
//
// 类型归 mesh 包(它就是 /api/mesh/* 的 wire 契约);servicereg 只引用类型,不反向依赖。
package mesh

import (
	"context"
	"net/http"
)

// ServiceCatalog 是 /api/mesh/servicereg/catalog 的响应载荷。节点身份由拉取方自己的
// peer 注册表提供(它按 nodeID 发起拉取),载荷内不再冗余。
type ServiceCatalog struct {
	// HTTPPort / HTTPSPort 是本端网关的 HTTP/HTTPS 宿主端口(转发入口端口;对端据此
	// 渲染 upstream 成员 —— 有证书服务走 HTTPSPort + SNI,无证书走 HTTPPort)。
	HTTPPort  int              `json:"httpPort"`
	HTTPSPort int              `json:"httpsPort"`
	// Gateways 是本端全部网关主机(IP 为控制端解析出的可达地址;空 = 解析失败,
	// 转发端跳过该台并在状态里标注)。
	Gateways []CatalogGateway `json:"gateways"`
	// Services 是可被对端转发的已启用服务(static/container/address;forward 服务
	// 不进目录 —— 禁止链式转发,防环)。
	Services []CatalogService `json:"services"`
}

// CatalogGateway 是目录里的一台网关主机。
type CatalogGateway struct {
	ServerID   string `json:"serverId"`
	ServerName string `json:"serverName"`
	IP         string `json:"ip"`
}

// CatalogService 是目录里的一条可转发服务。
type CatalogService struct {
	ServiceID string `json:"serviceId"`
	// FQDN 是服务完整域名(http 转发时的 Host 头 / SNI)。
	FQDN string `json:"fqdn"`
	Path string `json:"path"`
	// Protocol 是 http | tcp。
	Protocol string `json:"protocol"`
	// HasCert 表示该服务基域是否已配证书(http:决定对端转 443+SNI 还是 80)。
	HasCert bool `json:"hasCert"`
	// TCPListenPort 是 tcp 服务在本端网关的监听端口(tcp 转发的对端端口)。
	TCPListenPort int `json:"tcpListenPort"`
}

// CatalogPath 是 URL 子路径展示('' = 根路径),与 servicereg 的规范形态一致。
func (c CatalogService) CatalogPath() string {
	if c.Path == "" {
		return "/"
	}
	return c.Path
}

// FetchServiceCatalog 拉取对端服务目录(GET /api/mesh/servicereg/catalog)。
// 对端 forward ACL 未授权 → ErrForbidden;网络不可达 → ErrPeerUnreachable。
func (c *Client) FetchServiceCatalog(ctx context.Context, p *Peer) (*ServiceCatalog, error) {
	var out ServiceCatalog
	if err := c.Do(ctx, p, http.MethodGet, "/api/mesh/servicereg/catalog", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
