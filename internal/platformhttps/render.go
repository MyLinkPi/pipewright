package platformhttps

import (
	"strconv"
	"strings"
)

// 远端文件布局(与用户已有配置共存,平台只动这两个路径)。
const (
	// confMarker 是配置内的唯一标记:nginx -T 验证 include 生效时 grep 此串。
	confMarker = "pipewright-platform-https"
	// managedConfPath 是平台写入的 vhost 落点(Debian/Ubuntu/CentOS/Alpine 发行版 nginx 默认
	// include /etc/nginx/conf.d/*.conf)。
	managedConfPath = "/etc/nginx/conf.d/pipewright-platform.conf"
	// certsBaseDir 是证书落盘根目录:certs/<域名>/{fullchain,privkey}.pem。
	certsBaseDir = "/etc/nginx/pipewright/certs"
)

// certDir 返回某域名的证书目录。
func certDir(domain string) string { return certsBaseDir + "/" + domain }

// renderPlatformConf 渲染平台 HTTPS 的 vhost 配置(纯函数,可直接 golden 单测)。
//
// 结构:
//
//	map $http_upgrade $connection_upgrade   —— WebSocket 终端(交互终端走 WS)
//	server 80                                —— httpRedirect 开:return 301 https://$host$request_uri;
//	                                         关:与 443 同款反代(X-Forwarded-Proto http)
//	server 443 ssl                           —— ssl_certificate 挂 certs/<域名>/{fullchain,privkey}.pem,
//	                                         proxy_pass http://<upstream>;SSE 日志流需长读超时
//
// 所有进入文本的值已在领域层严格校验过(域名/上游白名单),渲染处不再二次防注入。
func renderPlatformConf(domain, upstreamHost string, upstreamPort int, httpRedirect bool) string {
	target := "http://" + upstreamHost + ":" + strconv.Itoa(upstreamPort)
	certDir := certDir(domain)

	var b strings.Builder
	b.WriteString("# " + confMarker + ":由 Pipewright 自动生成(平台 HTTPS 访问),请勿手改。\n")
	b.WriteString("map $http_upgrade $connection_upgrade {\n")
	b.WriteString("    default upgrade;\n")
	b.WriteString("    ''      close;\n")
	b.WriteString("}\n\n")

	// 80 块:跳转(默认)或与 443 同款反代。
	b.WriteString("server {\n")
	b.WriteString("    listen 80;\n")
	b.WriteString("    server_name " + domain + ";\n\n")
	if httpRedirect {
		b.WriteString("    return 301 https://$host$request_uri;\n")
	} else {
		writeProxyLocation(&b, "    ", target, "http")
	}
	b.WriteString("}\n\n")

	// 443 块:TLS 终结 + 反代平台 Web。
	b.WriteString("server {\n")
	b.WriteString("    listen 443 ssl;\n")
	b.WriteString("    server_name " + domain + ";\n\n")
	b.WriteString("    ssl_certificate     " + certDir + "/fullchain.pem;\n")
	b.WriteString("    ssl_certificate_key " + certDir + "/privkey.pem;\n")
	b.WriteString("    ssl_protocols TLSv1.2 TLSv1.3;\n\n")
	b.WriteString("    client_max_body_size 512m;\n\n")
	writeProxyLocation(&b, "    ", target, "https")
	b.WriteString("}\n")
	return b.String()
}

// writeProxyLocation 输出反代 location 块(proto 决定 X-Forwarded-Proto;长读超时护 SSE 日志流)。
func writeProxyLocation(b *strings.Builder, indent, target, proto string) {
	b.WriteString(indent + "location / {\n")
	b.WriteString(indent + "    proxy_pass " + target + ";\n")
	b.WriteString(indent + "    proxy_http_version 1.1;\n")
	b.WriteString(indent + "    proxy_set_header Host $host;\n")
	b.WriteString(indent + "    proxy_set_header X-Real-IP $remote_addr;\n")
	b.WriteString(indent + "    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;\n")
	b.WriteString(indent + "    proxy_set_header X-Forwarded-Proto " + proto + ";\n")
	b.WriteString(indent + "    proxy_set_header Upgrade $http_upgrade;\n")
	b.WriteString(indent + "    proxy_set_header Connection $connection_upgrade;\n")
	b.WriteString(indent + "    # SSE 日志流/长连接需要较长读超时。\n")
	b.WriteString(indent + "    proxy_read_timeout 3600s;\n")
	b.WriteString(indent + "    proxy_send_timeout 3600s;\n")
	b.WriteString(indent + "}\n")
}
