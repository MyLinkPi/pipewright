package servicereg

import (
	"context"
	"database/sql"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/store"
	"github.com/huangchengsir/pipewright/internal/storetest"
	"github.com/huangchengsir/pipewright/internal/target"
)

// --- 渲染单测(纯函数) ----------------------------------------------------

func TestRenderNginxConfEmpty(t *testing.T) {
	out := renderNginxConf(nil, nil, nil, nil, 0)
	if strings.Contains(out, "ssl_certificate") || strings.Contains(out, "stream {") {
		t.Fatalf("空输入不应渲染证书/流块:\n%s", out)
	}
	if !strings.Contains(out, "listen 80 default_server;") {
		t.Fatalf("空配置仍应有 80 兜底 server:\n%s", out)
	}
	if !strings.Contains(out, "resolver 127.0.0.11 valid=10s") {
		t.Fatalf("应始终渲染 Docker 内嵌 DNS resolver:\n%s", out)
	}
}

func TestRenderNginxConfHTTPS(t *testing.T) {
	dom := Domain{ID: "d1", BaseDomain: "efg.com", HasCert: true}
	svc := RegisteredService{ID: "s1", Name: "abc", DomainID: "d1", Protocol: ProtocolHTTP,
		UpstreamKind: UpstreamKindContainer, Upstream: "web", UpstreamPort: 8080, Enabled: true}
	inst := Instance{ID: "i1", ServiceID: "s1", ServerID: "sv1", Container: "web", Port: 8080, HostPort: 20001, Attached: true}
	out := renderNginxConf([]Domain{dom}, []RegisteredService{svc}, []Instance{inst}, map[string]string{"sv1": "10.0.0.5"}, 0)

	mustContain := []string{
		"server_name abc.efg.com;",
		"listen 443 ssl;",
		"ssl_certificate /etc/pipewright/certs/efg.com/fullchain.pem;",
		"ssl_certificate_key /etc/pipewright/certs/efg.com/privkey.pem;",
		"upstream pw_abc {",
		"server 10.0.0.5:20001 max_fails=2 fail_timeout=10s;",
		"proxy_pass http://pw_abc;",
		"proxy_set_header X-Forwarded-Proto https;",
		// 80 → 443 跳转名单应含该 FQDN。
		"server_name abc.efg.com;\n        return 301 https://$host$request_uri;",
		// WebSocket 头。
		"proxy_set_header Upgrade $http_upgrade;",
	}
	for _, want := range mustContain {
		if !strings.Contains(out, want) {
			t.Fatalf("缺片段 %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "load_module") {
		t.Fatalf("无 tcp 服务不应加载 stream 模块:\n%s", out)
	}
}

func TestRenderNginxConfMultiInstance(t *testing.T) {
	dom := Domain{ID: "d1", BaseDomain: "efg.com", HasCert: true}
	svc := RegisteredService{ID: "s1", Name: "abc", DomainID: "d1", Protocol: ProtocolHTTP,
		UpstreamKind: UpstreamKindContainer, Upstream: "abc-1", UpstreamPort: 8080, Enabled: true}
	// 故意乱序输入:实例按(服务器, 容器)升序渲染,detached / 地址未知 / 未发布宿主端口的
	// 容器实例不进池。
	insts := []Instance{
		{ID: "i2", ServiceID: "s1", ServerID: "sv2", Container: "abc-2", HostPort: 20002, Attached: true},
		{ID: "i1", ServiceID: "s1", ServerID: "sv1", Container: "abc-1", Port: 9090, HostPort: 20001, Attached: true},
		{ID: "i3", ServiceID: "s1", ServerID: "sv1", Container: "abc-3", Attached: false},
		{ID: "i4", ServiceID: "s1", ServerID: "sv9", Container: "abc-4", Attached: true}, // 地址未解析 → 跳过
		{ID: "i5", ServiceID: "s1", ServerID: "", Container: "abc-5", Attached: true},    // 遗留行 → 跳过
		{ID: "i6", ServiceID: "s1", ServerID: "sv2", Container: "abc-6", Port: 8080, Attached: true}, // 容器实例无宿主端口 → 跳过(死成员)
	}
	addrs := map[string]string{"sv1": "10.0.0.1", "sv2": "10.0.0.2"}
	out := renderNginxConf([]Domain{dom}, []RegisteredService{svc}, insts, addrs, 3)
	if !strings.Contains(out, "upstream pw_abc {") {
		t.Fatalf("应渲染 upstream 块:\n%s", out)
	}
	// abc-1(svc sv1,host 20001);abc-2(host 20002);abc-3 摘除/abc-4 无地址/abc-5 遗留/abc-6 无宿主端口不出现。
	for _, want := range []string{
		"server 10.0.0.1:20001 max_fails=2 fail_timeout=10s;",
		"server 10.0.0.2:20002 max_fails=2 fail_timeout=10s;",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("实例成员不符(缺 %q):\n%s", want, out)
		}
	}
	// abc-6 的端口回落(10.0.0.2:8080 = 宿主无监听的死成员)不得渲染。
	if strings.Contains(out, "10.0.0.2:8080") {
		t.Fatalf("未发布宿主端口的容器实例不应渲染:\n%s", out)
	}
	for _, bad := range []string{"abc-3", "abc-4", "abc-5"} {
		if strings.Contains(out, bad) {
			t.Fatalf("%s 不应渲染:\n%s", bad, out)
		}
	}
	if !strings.Contains(out, "有 3 个实例因遗留数据") {
		t.Fatalf("跳过实例数应写入头注释:\n%s", out)
	}
	i1 := strings.Index(out, "server 10.0.0.1:")
	i2 := strings.Index(out, "server 10.0.0.2:")
	if i1 < 0 || i2 < 0 || i1 > i2 {
		t.Fatalf("实例应按服务器升序:\n%s", out)
	}
}

func TestRenderNginxConfAllInstancesDetached503(t *testing.T) {
	dom := Domain{ID: "d1", BaseDomain: "efg.com", HasCert: false}
	svc := RegisteredService{ID: "s1", Name: "abc", DomainID: "d1", Protocol: ProtocolHTTP,
		UpstreamKind: UpstreamKindContainer, Upstream: "abc-1", UpstreamPort: 8080, Enabled: true}
	out := renderNginxConf([]Domain{dom}, []RegisteredService{svc}, nil, nil, 0)
	if !strings.Contains(out, "server_name abc.efg.com;") {
		t.Fatalf("server 块应保留:\n%s", out)
	}
	if !strings.Contains(out, "return 503;") {
		t.Fatalf("实例全摘除应 503 维护态:\n%s", out)
	}
	if strings.Contains(out, "upstream pw_abc") || strings.Contains(out, "proxy_pass") {
		t.Fatalf("无实例不应有 upstream/proxy_pass:\n%s", out)
	}
}

func TestRenderNginxConfAddressServiceWithInstances(t *testing.T) {
	dom := Domain{ID: "d1", BaseDomain: "efg.com", HasCert: false}
	// address 服务(非容器实例):有实例 → 走集群实例池;无实例 → 保留单上游变量动态解析。
	svc := RegisteredService{ID: "s1", Name: "abc", DomainID: "d1", Protocol: ProtocolHTTP,
		UpstreamKind: UpstreamKindAddress, Upstream: "10.0.0.5", UpstreamPort: 3000, Enabled: true}
	plain := RegisteredService{ID: "s2", Name: "plain", DomainID: "d1", Protocol: ProtocolHTTP,
		UpstreamKind: UpstreamKindAddress, Upstream: "10.0.0.9", UpstreamPort: 3001, Enabled: true}
	insts := []Instance{{ID: "i1", ServiceID: "s1", ServerID: "app1", Container: "", Port: 8080, HostPort: 8080, Attached: true}}
	out := renderNginxConf([]Domain{dom}, []RegisteredService{svc, plain}, insts, map[string]string{"app1": "10.0.0.7"}, 0)
	if !strings.Contains(out, "upstream pw_abc {") || !strings.Contains(out, "server 10.0.0.7:8080 max_fails=2 fail_timeout=10s;") {
		t.Fatalf("非容器实例应进集群实例池:\n%s", out)
	}
	if !strings.Contains(out, "set $up_plain 10.0.0.9:3001;") {
		t.Fatalf("无实例的 address 服务应保留单上游动态解析:\n%s", out)
	}
}

// TestRenderNginxConfAddressEmptyUpstreamMaintenance 非容器「一键生成」形态:http+address
// 空上游(部署托管)+ 无实例 → 503 维护态;绝不能渲染出 "set $up_abc :8080" 的坏配置。
func TestRenderNginxConfAddressEmptyUpstreamMaintenance(t *testing.T) {
	dom := Domain{ID: "d1", BaseDomain: "efg.com", HasCert: false}
	svc := RegisteredService{ID: "s1", Name: "abc", DomainID: "d1", Protocol: ProtocolHTTP,
		UpstreamKind: UpstreamKindAddress, Upstream: "", UpstreamPort: 8080, Enabled: true}
	out := renderNginxConf([]Domain{dom}, []RegisteredService{svc}, nil, nil, 0)
	if !strings.Contains(out, "return 503;") {
		t.Fatalf("空上游 address 服务应维护态:\n%s", out)
	}
	if strings.Contains(out, "set $up_abc") {
		t.Fatalf("空上游不应渲染变量反代:\n%s", out)
	}
	// 有 attached 实例后恢复正常反代(非容器实例 HostPort=0 继承 Port)。
	inst := Instance{ID: "i1", ServiceID: "s1", ServerID: "app1", Port: 8080, Attached: true}
	out = renderNginxConf([]Domain{dom}, []RegisteredService{svc}, []Instance{inst}, map[string]string{"app1": "10.0.0.7"}, 0)
	if !strings.Contains(out, "server 10.0.0.7:8080 max_fails=2 fail_timeout=10s;") {
		t.Fatalf("实例上线后应进实例池:\n%s", out)
	}
}

// TestRenderNginxConfIPv6Bracketed IPv6 服务器地址 / 上游必须带方括号(裸拼冒号 nginx -t 失败,
// 会卡死整台网关的 apply)。
func TestRenderNginxConfIPv6Bracketed(t *testing.T) {
	dom := Domain{ID: "d1", BaseDomain: "efg.com", HasCert: false}
	svc := RegisteredService{ID: "s1", Name: "abc", DomainID: "d1", Protocol: ProtocolHTTP,
		UpstreamKind: UpstreamKindContainer, Upstream: "web", UpstreamPort: 8080, Enabled: true}
	inst := Instance{ID: "i1", ServiceID: "s1", ServerID: "sv6", Container: "web", HostPort: 20001, Attached: true}
	out := renderNginxConf([]Domain{dom}, []RegisteredService{svc}, []Instance{inst}, map[string]string{"sv6": "fd00::1"}, 0)
	if !strings.Contains(out, "server [fd00::1]:20001 max_fails=2 fail_timeout=10s;") {
		t.Fatalf("IPv6 实例成员应加方括号:\n%s", out)
	}
	// address 形态的 IPv6 上游同样加方括号。
	v6 := RegisteredService{ID: "s2", Name: "v6", DomainID: "d1", Protocol: ProtocolHTTP,
		UpstreamKind: UpstreamKindAddress, Upstream: "fd00::9", UpstreamPort: 3000, Enabled: true}
	out = renderNginxConf([]Domain{dom}, []RegisteredService{v6}, nil, nil, 0)
	if !strings.Contains(out, "set $up_v6 [fd00::9]:3000;") {
		t.Fatalf("IPv6 上游应加方括号:\n%s", out)
	}
}

func TestRenderNginxConfHTTPOnlyNoCert(t *testing.T) {
	dom := Domain{ID: "d1", BaseDomain: "efg.com", HasCert: false}
	svc := RegisteredService{Name: "abc", DomainID: "d1", Protocol: ProtocolHTTP,
		UpstreamKind: UpstreamKindAddress, Upstream: "10.0.0.5", UpstreamPort: 3000, Enabled: true}
	out := renderNginxConf([]Domain{dom}, []RegisteredService{svc}, nil, nil, 0)
	if strings.Contains(out, "listen 443") {
		t.Fatalf("无证书基域不应渲染 443:\n%s", out)
	}
	if !strings.Contains(out, "server_name abc.efg.com;") {
		t.Fatalf("应有 80 server 块:\n%s", out)
	}
	if !strings.Contains(out, "set $up_abc 10.0.0.5:3000;") {
		t.Fatalf("address 上游应直接反代:\n%s", out)
	}
	if !strings.Contains(out, "X-Forwarded-Proto http;") {
		t.Fatalf("明文 HTTP 应标记 proto http:\n%s", out)
	}
}

func TestRenderNginxConfTCP(t *testing.T) {
	dom := Domain{ID: "d1", BaseDomain: "efg.com", HasCert: false}
	svc := RegisteredService{Name: "my-db", DomainID: "d1", Protocol: ProtocolTCP,
		UpstreamKind: UpstreamKindContainer, Upstream: "mysql", UpstreamPort: 3306,
		TCPListenPort: 13306, Enabled: true}
	out := renderNginxConf([]Domain{dom}, []RegisteredService{svc}, nil, nil, 0)
	for _, want := range []string{
		"load_module /usr/lib/nginx/modules/ngx_stream_module.so;",
		"stream {",
		"listen 13306;",
		"set $up_my_db mysql:3306;",
		"proxy_pass $up_my_db;",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("TCP 块缺片段 %q:\n%s", want, out)
		}
	}
}

func TestRenderNginxConfSortedDeterministic(t *testing.T) {
	dom1 := Domain{ID: "d1", BaseDomain: "aaa.com", HasCert: true}
	dom2 := Domain{ID: "d2", BaseDomain: "zzz.com", HasCert: true}
	mk := func(name, domID string) RegisteredService {
		return RegisteredService{ID: "s-" + name, Name: name, DomainID: domID, Protocol: ProtocolHTTP,
			UpstreamKind: UpstreamKindContainer, Upstream: name + "c", UpstreamPort: 80, Enabled: true}
	}
	insts := []Instance{
		{ID: "i1", ServiceID: "s-alpha", ServerID: "sv1", Container: "alphac", Attached: true},
		{ID: "i2", ServiceID: "s-zeta", ServerID: "sv2", Container: "zetac", Attached: true},
	}
	addrs := map[string]string{"sv1": "10.0.0.1", "sv2": "10.0.0.2"}
	services := []RegisteredService{mk("zeta", "d2"), mk("alpha", "d1")}
	a := renderNginxConf([]Domain{dom1, dom2}, services, insts, addrs, 0)
	// 输入顺序无关:打乱后重渲染结果应逐字节一致。
	services2 := []RegisteredService{mk("alpha", "d1"), mk("zeta", "d2")}
	insts2 := []Instance{insts[1], insts[0]}
	b := renderNginxConf([]Domain{dom2, dom1}, services2, insts2, addrs, 0)
	if a != b {
		t.Fatalf("渲染应确定性(输入顺序无关):\n--- a ---\n%s\n--- b ---\n%s", a, b)
	}
	ai := strings.Index(a, "alpha.aaa.com")
	zi := strings.Index(a, "zeta.zzz.com")
	if ai < 0 || zi < 0 || ai > zi {
		t.Fatalf("应按 FQDN 升序:\n%s", a)
	}
}

func TestRenderNginxConfDisabledSkipped(t *testing.T) {
	dom := Domain{ID: "d1", BaseDomain: "efg.com", HasCert: true}
	svc := RegisteredService{Name: "abc", DomainID: "d1", Protocol: ProtocolHTTP,
		UpstreamKind: UpstreamKindContainer, Upstream: "web", UpstreamPort: 8080, Enabled: false}
	out := renderNginxConf([]Domain{dom}, []RegisteredService{svc}, nil, nil, 0)
	if strings.Contains(out, "abc.efg.com") {
		t.Fatalf("禁用服务不应渲染:\n%s", out)
	}
}

// --- 网关容器自举(回归:旧版自举配置把 return 放进 http 上下文,非法,容器崩溃循环) ---

func TestNginxBootstrapMinConfValid(t *testing.T) {
	// return 只允许 server/location/if 上下文;坏形态特征是 "http { return"。
	if strings.Contains(nginxBootstrapMinConf, "http { return") {
		t.Fatalf("自举配置不得在 http 上下文直接 return:\n%s", nginxBootstrapMinConf)
	}
	if !strings.Contains(nginxBootstrapMinConf, "http { server { return 404; } }") {
		t.Fatalf("自举配置应是最小合法形态(http>server>return 404):\n%s", nginxBootstrapMinConf)
	}
	// 自举脚本须字节精确识别旧版坏配置并重写,且保持前台主进程形态。
	for _, want := range []string{
		"| cmp -s - \"$conf\"",
		"printf '" + nginxBootstrapPoisonConf + "'",
		"printf '" + nginxBootstrapMinConf + "'",
		"exec nginx -c \"$conf\" -g \"daemon off;\"",
	} {
		if !strings.Contains(nginxBootstrapScript, want) {
			t.Fatalf("自举脚本缺片段 %q:\n%s", want, nginxBootstrapScript)
		}
	}
}

// gwContainerUp 是「容器在且端口映射吻合」的 fake 探测结果,label 为自举版本标签输出。
func gwContainerUp(label string) func([]string) *target.ExecResult {
	return func(cmd []string) *target.ExecResult {
		joined := strings.Join(cmd, " ")
		switch {
		case strings.Contains(joined, "PortBindings"):
			return &target.ExecResult{ExitCode: 0, Stdout: "80/tcp=80 443/tcp=443 "}
		case strings.Contains(joined, "Config.Labels"):
			return &target.ExecResult{ExitCode: 0, Stdout: label}
		}
		return nil
	}
}

func TestEnsureNginxRecreatesOnBootstrapVerMismatch(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc, ft := newTestService(t, st.DB)
		ctx := context.Background()
		if _, err := svc.UpdateSettings(ctx, SettingsUpdate{ServerID: strPtr("srv1")}); err != nil {
			t.Fatal(err)
		}
		// 容器在、端口吻合,但自举版本标签缺失(旧版容器,入口脚本烧死为坏配置自举)→ 重建。
		ft.resultFor = gwContainerUp("")
		if err := svc.Apply(ctx); err != nil {
			t.Fatalf("apply:%v", err)
		}
		if !hasCmd(ft, "docker", "rm", "-f", "pipewright-nginx") {
			t.Fatalf("标签不符应重建容器:\n%s", joinAllCmds(ft))
		}
		hasLabeledRun := false
		for _, c := range ft.execCalls {
			if strings.HasPrefix(strings.Join(c, " "), "docker run") &&
				strings.Contains(strings.Join(c, " "), "--label pipewright.bootstrap=2") {
				hasLabeledRun = true
			}
		}
		if !hasLabeledRun {
			t.Fatalf("重建容器应带自举版本标签:\n%s", joinAllCmds(ft))
		}
	})
}

func TestEnsureNginxKeepsContainerWhenPortAndLabelMatch(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc, ft := newTestService(t, st.DB)
		ctx := context.Background()
		if _, err := svc.UpdateSettings(ctx, SettingsUpdate{ServerID: strPtr("srv1")}); err != nil {
			t.Fatal(err)
		}
		ft.resultFor = gwContainerUp(nginxBootstrapVer)
		if err := svc.Apply(ctx); err != nil {
			t.Fatalf("apply:%v", err)
		}
		if hasCmd(ft, "docker", "rm", "-f") || hasCmd(ft, "docker", "run") {
			t.Fatalf("端口与自举标签均吻合不应重建:\n%s", joinAllCmds(ft))
		}
		// 不重建时配置下发链仍应完整(nginx -t → cp → reload)。
		for _, prefix := range [][]string{
			{"docker", "exec", "pipewright-nginx", "nginx", "-t", "-c"},
			{"docker", "cp", "/tmp/pipewright-nginx.conf"},
			{"docker", "exec", "pipewright-nginx", "nginx", "-s", "reload"},
		} {
			if !hasCmd(ft, prefix...) {
				t.Fatalf("缺命令 %v:\n%s", prefix, joinAllCmds(ft))
			}
		}
	})
}

// --- 校验单测 --------------------------------------------------------------

func TestValidateBaseDomain(t *testing.T) {
	for _, ok := range []string{"efg.com", "a-b.co.uk", "x.io"} {
		if err := validateBaseDomain(ok); err != nil {
			t.Fatalf("%q 应合法:%v", ok, err)
		}
	}
	for _, bad := range []string{"", "*.efg.com", "efg", "-a.com", "a-.com", "e fg.com", "EFG.COM", "a_b.com"} {
		if err := validateBaseDomain(bad); err == nil {
			t.Fatalf("%q 应非法", bad)
		}
	}
}

func TestValidUpstreamHost(t *testing.T) {
	for _, ok := range []string{"host.docker.internal", "10.0.0.5", "db.internal", "example.com"} {
		if !validUpstreamHost(ok) {
			t.Fatalf("%q 应合法", ok)
		}
	}
	for _, bad := range []string{"", "a b", "10.0.0.5:80", "x;rm", "-host"} {
		if validUpstreamHost(bad) {
			t.Fatalf("%q 应非法", bad)
		}
	}
}

func TestValidateSettings(t *testing.T) {
	good := Settings{HTTPPort: 80, HTTPSPort: 443, Image: "nginx:stable-alpine",
		Network: "pipewright-gateway", ContainerName: "pipewright-nginx", VolumeName: "pipewright_nginx"}
	if err := validateSettings(good); err != nil {
		t.Fatalf("默认设置应合法:%v", err)
	}
	bads := []Settings{
		{HTTPPort: 0, HTTPSPort: 443},                 // 端口越界
		{HTTPPort: 80, HTTPSPort: 80},                  // 端口相同
		{HTTPPort: 80, HTTPSPort: 443, Image: "-x"},    // 镜像 flag 注入
		{HTTPPort: 80, HTTPSPort: 443, Network: "a b"}, // 网络名非法
	}
	for i, b := range bads {
		b.Image = orDefaultStr(b.Image, good.Image)
		b.Network = orDefaultStr(b.Network, good.Network)
		b.ContainerName = orDefaultStr(b.ContainerName, good.ContainerName)
		b.VolumeName = orDefaultStr(b.VolumeName, good.VolumeName)
		if err := validateSettings(b); err == nil {
			t.Fatalf("第 %d 组应非法:%+v", i, b)
		}
	}
}

func orDefaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// --- 领域服务单测(fake target + 透传 sealer + storetest 双方言) -----------

// fakeSealer 是明文透传的假保险库(仅测流转,不测加密本身)。
type fakeSealer struct{}

func (fakeSealer) SealSecret(p []byte) ([]byte, error) { return append([]byte("sealed:"), p...), nil }
func (fakeSealer) OpenSecret(s []byte) ([]byte, error) {
	return []byte(strings.TrimPrefix(string(s), "sealed:")), nil
}

// fakeTarget 是捕获 Exec/Upload 的假 target.Service(照抄 proxy 包测试桩)。
type fakeTarget struct {
	execCalls       [][]string
	execServers     []string // 与 execCalls 一一对应:该次命令发往的 serverID
	uploads         []string
	uploadServers   []string // 与 uploads 一一对应:该次上传发往的 serverID
	uploadBytes     map[string]string
	resultFor       func(cmd []string) *target.ExecResult
	resultForServer func(serverID string, cmd []string) *target.ExecResult // 优先于 resultFor
}


// DockerLogin 满足 target.Service 接口(部署私有仓库登录追加);测试桩不触网,直接成功。
func (f *fakeTarget) DockerLogin(context.Context, string, string, string) error { return nil }
func (f *fakeTarget) Exec(_ context.Context, serverID string, cmd []string) (*target.ExecResult, error) {
	f.execCalls = append(f.execCalls, cmd)
	f.execServers = append(f.execServers, serverID)
	if f.resultForServer != nil {
		if r := f.resultForServer(serverID, cmd); r != nil {
			return r, nil
		}
	}
	if f.resultFor != nil {
		if r := f.resultFor(cmd); r != nil {
			return r, nil
		}
	}
	return &target.ExecResult{ExitCode: 0}, nil
}

// ExecWithStdin 满足 target.Service 接口(sudo -S 追加);servicereg 不用 stdin,转发 Exec 语义。
func (f *fakeTarget) ExecWithStdin(ctx context.Context, id string, cmd []string, stdin io.Reader) (*target.ExecResult, error) {
	_, _ = io.Copy(io.Discard, stdin)
	return f.Exec(ctx, id, cmd)
}

func (f *fakeTarget) Upload(_ context.Context, serverID string, content io.Reader, remotePath string) error {
	f.uploads = append(f.uploads, remotePath)
	f.uploadServers = append(f.uploadServers, serverID)
	b, _ := io.ReadAll(content)
	if f.uploadBytes == nil {
		f.uploadBytes = map[string]string{}
	}
	f.uploadBytes[remotePath] = string(b)
	return nil
}

func (f *fakeTarget) Get(context.Context, string) (*target.Server, error) { return nil, nil }
func (f *fakeTarget) List(context.Context) ([]*target.Server, error)      { return nil, nil }
func (f *fakeTarget) Create(context.Context, target.CreateInput) (*target.Server, error) {
	return nil, nil
}
func (f *fakeTarget) Update(context.Context, string, target.UpdateInput) (*target.Server, error) {
	return nil, nil
}
func (f *fakeTarget) Delete(context.Context, string) error                     { return nil }
func (f *fakeTarget) Test(context.Context, string) (*target.TestResult, error) { return nil, nil }
func (f *fakeTarget) ExecStream(context.Context, string, []string) (io.ReadCloser, error) {
	return nil, nil
}
func (f *fakeTarget) ExecInteractive(context.Context, string, []string) (target.Session, error) {
	return nil, nil
}

func hasCmd(f *fakeTarget, prefix ...string) bool {
	joined := strings.Join(prefix, " ")
	for _, c := range f.execCalls {
		if strings.HasPrefix(strings.Join(c, " "), joined) {
			return true
		}
	}
	return false
}

// hasCmdOn 报告是否向指定主机发过匹配前缀的命令(多机 apply 断言用)。
func hasCmdOn(f *fakeTarget, serverID string, prefix ...string) bool {
	joined := strings.Join(prefix, " ")
	for i, c := range f.execCalls {
		if f.execServers[i] == serverID && strings.HasPrefix(strings.Join(c, " "), joined) {
			return true
		}
	}
	return false
}

// uploadToOn 报告是否向指定主机上传过某路径(多机 apply 断言用)。
func uploadToOn(f *fakeTarget, serverID, path string) bool {
	for i, u := range f.uploads {
		if u == path && f.uploadServers[i] == serverID {
			return true
		}
	}
	return false
}

// newTestServiceMulti 同 newTestService,但网关配置为多台主机。
func newTestServiceMulti(t *testing.T, db *sql.DB, serverIDs ...string) (Service, *fakeTarget) {
	t.Helper()
	svc, ft := newTestService(t, db)
	if _, err := svc.UpdateSettings(context.Background(), SettingsUpdate{ServerIDs: &serverIDs}); err != nil {
		t.Fatalf("配置多网关:%v", err)
	}
	return svc, ft
}

func newTestService(t *testing.T, db *sql.DB) (Service, *fakeTarget) {
	t.Helper()
	ft := &fakeTarget{}
	svc := New(db, ft, fakeSealer{})
	// 集群渲染测试桩:任意 serverID → 固定可达 IP(apply 级测试渲染实例成员需要)。
	svc.SetServerAddrResolver(func(context.Context, string) (string, error) { return "10.9.9.9", nil })
	return svc, ft
}

func TestDomainCRUDAndServiceCreate(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc, ft := newTestService(t, st.DB)
		ctx := context.Background()

		// 网关未配置:注册域/服务为纯配置态,不产生任何 SSH 命令。
		dom, err := svc.CreateDomain(ctx, "EFG.COM")
		if err != nil {
			t.Fatalf("注册基域:%v", err)
		}
		if dom.BaseDomain != "efg.com" {
			t.Fatalf("基域应小写归一:%q", dom.BaseDomain)
		}
		if _, err := svc.CreateDomain(ctx, "efg.com"); err != ErrDomainTaken {
			t.Fatalf("重复基域应 ErrDomainTaken,得 %v", err)
		}
		if len(ft.execCalls) != 0 {
			t.Fatalf("网关未配置时不应有编排命令:%v", ft.execCalls)
		}

		rs, err := svc.CreateService(ctx, CreateServiceInput{
			DomainID: dom.ID, Name: "ABC", Protocol: ProtocolHTTP,
			UpstreamKind: UpstreamKindContainer, Upstream: "web", UpstreamPort: 8080,
		})
		if err != nil {
			t.Fatalf("注册服务:%v", err)
		}
		if rs.Name != "abc" {
			t.Fatalf("服务名应小写归一:%q", rs.Name)
		}
		if _, err := svc.CreateService(ctx, CreateServiceInput{
			DomainID: dom.ID, Name: "abc", Protocol: ProtocolHTTP,
			UpstreamKind: UpstreamKindContainer, Upstream: "web2", UpstreamPort: 80,
		}); err != ErrServiceTaken {
			t.Fatalf("同名服务应 ErrServiceTaken,得 %v", err)
		}
		// 非法输入。
		if _, err := svc.CreateService(ctx, CreateServiceInput{DomainID: dom.ID, Name: "-bad", UpstreamPort: 80}); err != ErrInvalidServiceName {
			t.Fatalf("-bad 应 ErrInvalidServiceName,得 %v", err)
		}
		if _, err := svc.CreateService(ctx, CreateServiceInput{DomainID: dom.ID, Name: "x", Upstream: "web", UpstreamPort: 0}); err != ErrInvalidPort {
			t.Fatalf("端口 0 应 ErrInvalidPort,得 %v", err)
		}

		list, err := svc.ListServices(ctx)
		if err != nil || len(list) != 1 || list[0].BaseDomain != "efg.com" {
			t.Fatalf("列表不符:%v %v", list, err)
		}
	})
}

func TestServiceFQDNUniqueAcrossDomains(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc, _ := newTestService(t, st.DB)
		ctx := context.Background()
		d1, _ := svc.CreateDomain(ctx, "efg.com")
		d2, _ := svc.CreateDomain(ctx, "x.efg.com")
		if _, err := svc.CreateService(ctx, CreateServiceInput{DomainID: d1.ID, Name: "abc", Upstream: "web", UpstreamPort: 80}); err != nil {
			t.Fatalf("首个 abc 应成功:%v", err)
		}
		// abc.x.efg.com 与 abc.efg.com 同名不同域 → FQDN 不撞,允许。
		if _, err := svc.CreateService(ctx, CreateServiceInput{DomainID: d2.ID, Name: "abc", Upstream: "web", UpstreamPort: 80}); err != nil {
			t.Fatalf("不同基域同名服务不应冲突:%v", err)
		}
	})
}

func TestTCPPortConflict(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc, _ := newTestService(t, st.DB)
		ctx := context.Background()
		dom, _ := svc.CreateDomain(ctx, "efg.com")
		mk := func(name string, port int) CreateServiceInput {
			return CreateServiceInput{DomainID: dom.ID, Name: name, Protocol: ProtocolTCP,
				Upstream: "db", UpstreamPort: 3306, TCPListenPort: port}
		}
		if _, err := svc.CreateService(ctx, CreateServiceInput{DomainID: dom.ID, Name: "a", Protocol: ProtocolTCP, Upstream: "db", UpstreamPort: 3306, TCPListenPort: 80}); err != ErrTCPPortTaken {
			t.Fatalf("监听 80 应冲突(与 HTTP 冲突),得 %v", err)
		}
		if _, err := svc.CreateService(ctx, CreateServiceInput{DomainID: dom.ID, Name: "b", Protocol: ProtocolTCP, Upstream: "db", UpstreamPort: 3306, TCPListenPort: 443}); err != ErrTCPPortTaken {
			t.Fatalf("监听 443 应冲突,得 %v", err)
		}
		first, err := svc.CreateService(ctx, mk("db", 13306))
		if err != nil {
			t.Fatalf("首个 TCP 服务:%v", err)
		}
		_ = first
		if _, err := svc.CreateService(ctx, mk("cache", 13306)); err != ErrTCPPortTaken {
			t.Fatalf("重复监听端口应冲突,得 %v", err)
		}
	})
}

func TestUploadCertRoundtripAndApply(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc, ft := newTestService(t, st.DB)
		ctx := context.Background()
		dom, err := svc.CreateDomain(ctx, "efg.com")
		if err != nil {
			t.Fatal(err)
		}
		certPEM, keyPEM := selfSignedCertPEM(t, []string{"*.efg.com"})

		// 网关未配置:证书照常落库(apply 静默跳过)。
		upd, err := svc.UploadCert(ctx, dom.ID, certPEM, keyPEM)
		if err != nil {
			t.Fatalf("上传证书:%v", err)
		}
		if !upd.HasCert || upd.CertExpiresAt.IsZero() || !strings.Contains(upd.CertSubject, "efg.com") {
			t.Fatalf("证书元数据不符:%+v", upd)
		}

		// 配置网关后再传:应触发完整编排(ensure → cert 下发 → nginx -t → cp → reload)。
		if _, err := svc.UpdateSettings(ctx, SettingsUpdate{ServerID: strPtr("srv1")}); err != nil {
			t.Fatal(err)
		}
		ft.execCalls = nil
		if _, err := svc.UploadCert(ctx, dom.ID, certPEM, keyPEM); err != nil {
			t.Fatalf("带网关上传证书:%v", err)
		}
		for _, prefix := range [][]string{
			{"docker", "network", "inspect"},
			{"docker", "inspect", "pipewright-nginx"},
			{"docker", "exec", "pipewright-nginx", "mkdir", "-p", "/etc/pipewright/certs/efg.com"},
			{"docker", "cp", "/tmp/pipewright-nginx.conf", "pipewright-nginx:/tmp/pipewright-nginx.conf"},
			{"docker", "exec", "pipewright-nginx", "nginx", "-t", "-c"},
			{"docker", "cp", "/tmp/pipewright-nginx.conf", "pipewright-nginx:/etc/pipewright/nginx.conf"},
			{"docker", "exec", "pipewright-nginx", "nginx", "-s", "reload"},
		} {
			if !hasCmd(ft, prefix...) {
				t.Fatalf("缺命令 %v,实际:\n%s", prefix, joinAllCmds(ft))
			}
		}
		// 证书密文经 docker cp 下发(Upload 出现在证书与配置临时路径)。
		if !uploadTo(ft, "/tmp/pipewright-sr-cert-efg.com-privkey.pem") {
			t.Fatalf("应下发私钥文件:%v", ft.uploads)
		}

		// 错配的证书/私钥 → ErrInvalidCert。
		_, otherKey := selfSignedCertPEM(t, []string{"*.other.com"})
		if _, err := svc.UploadCert(ctx, dom.ID, certPEM, otherKey); err != ErrInvalidCert {
			t.Fatalf("错配应 ErrInvalidCert,得 %v", err)
		}
	})
}

func TestApplyNginxTestFailureKeepsConfig(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc, ft := newTestService(t, st.DB)
		ctx := context.Background()
		dom, _ := svc.CreateDomain(ctx, "efg.com")
		if _, err := svc.UpdateSettings(ctx, SettingsUpdate{ServerID: strPtr("srv1")}); err != nil {
			t.Fatal(err)
		}
		// nginx -t 校验失败 → ErrApply,且不应执行 cp 正式路径/reload。
		ft.resultFor = func(cmd []string) *target.ExecResult {
			if len(cmd) >= 4 && cmd[0] == "docker" && cmd[2] == "pipewright-nginx" && cmd[3] == "nginx" && cmd[4] == "-t" {
				return &target.ExecResult{ExitCode: 1, Stderr: "nginx: [emerg] bad directive"}
			}
			return nil
		}
		if _, err := svc.CreateService(ctx, CreateServiceInput{DomainID: dom.ID, Name: "abc", Upstream: "web", UpstreamPort: 80}); err == nil {
			t.Fatal("apply 失败应回滚(创建报错)")
		}
		if hasCmd(ft, "docker", "cp", "/tmp/pipewright-nginx.conf", "pipewright-nginx:/etc/pipewright/nginx.conf") {
			t.Fatalf("校验失败不应覆盖正式配置:\n%s", joinAllCmds(ft))
		}
		// 服务应已回滚删除。
		list, _ := svc.ListServices(ctx)
		if len(list) != 0 {
			t.Fatalf("编排失败的服务应回滚:%v", list)
		}
	})
}

func TestSettingsDefaultsAndUpdate(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc, _ := newTestService(t, st.DB)
		ctx := context.Background()
		cfg, err := svc.GetSettings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Image != defaultNginxImage || cfg.HTTPPort != 80 || cfg.HTTPSPort != 443 {
			t.Fatalf("默认设置不符:%+v", cfg)
		}
		port := 8080
		cfg, err = svc.UpdateSettings(ctx, SettingsUpdate{HTTPPort: &port})
		if err != nil || cfg.HTTPPort != 8080 {
			t.Fatalf("更新端口:%v %+v", err, cfg)
		}
		bad := "nginx;rm -rf /"
		if _, err := svc.UpdateSettings(ctx, SettingsUpdate{Image: &bad}); err != ErrInvalidSetting {
			t.Fatalf("注入镜像应拒绝,得 %v", err)
		}
	})
}

// --- 多机网关(设置兼容 / 逐台收敛 / 部署联动) --------------------------------

// TestSettingsServerIDsCompat 验证多机设置的读写与存量单机数据兼容:
// 旧行(server_ids 空)读出回退到 server_id 单值;写侧 server_id 恒同步第一台(降级安全)。
func TestSettingsServerIDsCompat(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc, _ := newTestService(t, st.DB)
		ctx := context.Background()

		// 默认:未配置(空列表)。
		cfg, err := svc.GetSettings(ctx)
		if err != nil || len(cfg.ServerIDs) != 0 {
			t.Fatalf("默认应未配置:%v %v", cfg, err)
		}

		// 存量兼容:模拟旧版行(server_ids 空,server_id 有值)→ 读出回退单元素。
		if _, err := st.DB.Exec(`UPDATE service_reg_settings SET server_ids = '', server_id = 'legacy-srv'`); err != nil {
			t.Fatal(err)
		}
		cfg, err = svc.GetSettings(ctx)
		if err != nil || len(cfg.ServerIDs) != 1 || cfg.ServerIDs[0] != "legacy-srv" {
			t.Fatalf("旧行应回退 server_id:%+v %v", cfg, err)
		}

		// 新入口:多台 + 归一化(去空白/去重)。
		ids := []string{" srv1 ", "srv2", "srv1", ""}
		if _, err := svc.UpdateSettings(ctx, SettingsUpdate{ServerIDs: &ids}); err != nil {
			t.Fatalf("多机设置:%v", err)
		}
		cfg, _ = svc.GetSettings(ctx)
		if len(cfg.ServerIDs) != 2 || cfg.ServerIDs[0] != "srv1" || cfg.ServerIDs[1] != "srv2" {
			t.Fatalf("应归一化为 [srv1 srv2]:%+v", cfg.ServerIDs)
		}

		// 写侧同步:server_id 恒为第一台,server_ids 为规范 JSON(旧二进制可降级读)。
		var legacyID, idsJSON string
		if err := st.DB.QueryRow(`SELECT server_id, server_ids FROM service_reg_settings WHERE id = 'default'`).Scan(&legacyID, &idsJSON); err != nil {
			t.Fatal(err)
		}
		if legacyID != "srv1" || idsJSON != `["srv1","srv2"]` {
			t.Fatalf("server_id 应同步第一台:%q %q", legacyID, idsJSON)
		}

		// 空列表 = 清空(转配置态);重复旧入口设置单台等价单元素列表。
		empty := []string{}
		if _, err := svc.UpdateSettings(ctx, SettingsUpdate{ServerIDs: &empty}); err != nil {
			t.Fatalf("清空:%v", err)
		}
		if _, err := svc.UpdateSettings(ctx, SettingsUpdate{ServerID: strPtr("only")}); err != nil {
			t.Fatalf("旧入口单机:%v", err)
		}
		if cfg, _ = svc.GetSettings(ctx); len(cfg.ServerIDs) != 1 || cfg.ServerIDs[0] != "only" {
			t.Fatalf("旧入口应等价单元素列表:%+v", cfg.ServerIDs)
		}
	})
}

// TestApplyMultiServerContinueOnError 双机 apply:srv2 起容器失败时 srv1 仍完整收敛,
// 且逐台错误记入 settings(单台失败不阻断其余台)。
func TestApplyMultiServerContinueOnError(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc, ft := newTestServiceMulti(t, st.DB, "srv1", "srv2")
		ctx := context.Background()
		dom, err := svc.CreateDomain(ctx, "efg.com")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.CreateService(ctx, CreateServiceInput{DomainID: dom.ID, Name: "abc", Upstream: "web", UpstreamPort: 80}); err != nil {
			t.Fatal(err)
		}

		// 清掉创建时首轮成功 apply 的记录,只断言注入故障后这一轮。
		ft.execCalls, ft.execServers = nil, nil
		ft.uploads, ft.uploadServers = nil, nil

		// srv2 的 docker 网络检查/创建失败(容器起不来);srv1 一切正常。
		ft.resultForServer = func(serverID string, cmd []string) *target.ExecResult {
			if serverID == "srv2" && len(cmd) >= 2 && cmd[0] == "docker" && cmd[1] == "network" {
				return &target.ExecResult{ExitCode: 1, Stderr: "cannot connect to docker"}
			}
			return nil
		}
		err = svc.Apply(ctx)
		if err == nil || !strings.Contains(err.Error(), "srv2") {
			t.Fatalf("apply 应聚合并报出 srv2 失败:%v", err)
		}

		// srv1 不受影响:完整走完配置下发链并 reload。
		for _, prefix := range [][]string{
			{"docker", "run", "-d"},
			{"docker", "exec", "pipewright-nginx", "nginx", "-t", "-c"},
			{"docker", "exec", "pipewright-nginx", "nginx", "-s", "reload"},
		} {
			if !hasCmdOn(ft, "srv1", prefix...) {
				t.Fatalf("srv1 应完成收敛,缺 %v:\n%s", prefix, joinAllCmds(ft))
			}
		}
		if !uploadToOn(ft, "srv1", "/tmp/pipewright-nginx.conf") {
			t.Fatalf("srv1 应下发配置:%v", ft.uploads)
		}
		// srv2 卡在 ensure:不应起容器/下发配置。
		if hasCmdOn(ft, "srv2", "docker", "run") || uploadToOn(ft, "srv2", "/tmp/pipewright-nginx.conf") {
			t.Fatalf("srv2 失败后不应继续编排:\n%s", joinAllCmds(ft))
		}

		// 逐台错误落库:仅 srv2;聚合文案含 srv2。
		cfg, gerr := svc.GetSettings(ctx)
		if gerr != nil {
			t.Fatal(gerr)
		}
		if cfg.LastApplyErrors["srv2"] == "" {
			t.Fatalf("应记录 srv2 逐台错误:%+v", cfg.LastApplyErrors)
		}
		if _, hit := cfg.LastApplyErrors["srv1"]; hit {
			t.Fatalf("srv1 成功不应有错误记录:%+v", cfg.LastApplyErrors)
		}
		if !strings.Contains(cfg.LastApplyError, "srv2") {
			t.Fatalf("聚合文案应含 srv2:%q", cfg.LastApplyError)
		}

		// 恢复后重新收敛:错误应清空。
		ft.resultForServer = nil
		ft.execCalls, ft.execServers = nil, nil
		if err := svc.Apply(ctx); err != nil {
			t.Fatalf("恢复后 apply:%v", err)
		}
		if cfg, _ = svc.GetSettings(ctx); len(cfg.LastApplyErrors) != 0 || cfg.LastApplyError != "" {
			t.Fatalf("成功后应清错:%+v %q", cfg.LastApplyErrors, cfg.LastApplyError)
		}
	})
}

// TestResolveDeployInstancesServerScoped 集群模型:反查按实例归属服务器过滤 —— 滚动升级
// 只动本机,绝不返回其它机器的实例;serviceRef(服务 ID)精确绑定优先于容器名反查。
func TestResolveDeployInstancesServerScoped(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc, _ := newTestServiceMulti(t, st.DB, "gw1", "gw2")
		ctx := context.Background()
		dom, _ := svc.CreateDomain(ctx, "efg.com")
		created, err := svc.CreateService(ctx, CreateServiceInput{DomainID: dom.ID, Name: "abc", Upstream: "abc-1", UpstreamPort: 8080})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.AddInstance(ctx, created.ID, "app1", "abc-1", 0, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.AddInstance(ctx, created.ID, "app2", "abc-2", 9090, 20002); err != nil {
			t.Fatal(err)
		}

		// 容器名反查:定位服务后只返回**该服务器**上的 attached 实例。
		refs, err := svc.ResolveDeployInstances(ctx, "app1", "", "abc-1")
		if err != nil || len(refs) != 1 || refs[0].Container != "abc-1" {
			t.Fatalf("app1 应只返回本机实例 abc-1:%v %v", refs, err)
		}
		// 其它机器反查同名容器:定位到同一服务后返回**该机器**的实例(滚动只动本机)。
		refs, _ = svc.ResolveDeployInstances(ctx, "app2", "", "abc-1")
		if len(refs) != 1 || refs[0].Container != "abc-2" {
			t.Fatalf("app2 应返回本机实例 abc-2:%v", refs)
		}
		// 没有任何实例的机器 → 空。
		if refs, _ := svc.ResolveDeployInstances(ctx, "app9", "", "abc-1"); len(refs) != 0 {
			t.Fatalf("无实例机器应空:%v", refs)
		}
		// serviceRef(服务 ID)精确绑定 + server 过滤。
		refs, err = svc.ResolveDeployInstances(ctx, "app2", created.ID, "")
		if err != nil || len(refs) != 1 || refs[0].Container != "abc-2" || refs[0].HostPort != 20002 {
			t.Fatalf("serviceRef 绑定应返回 app2 实例(含宿主端口):%v %v", refs, err)
		}
		if refs[0].ServiceName != "abc.efg.com" {
			t.Fatalf("ServiceName 应为 FQDN:%q", refs[0].ServiceName)
		}
		// 非容器实例(container='')同样按服务 ID 反查。
		if _, err := svc.AddInstance(ctx, created.ID, "app3", "", 8080, 8080); err != nil {
			t.Fatal(err)
		}
		refs, _ = svc.ResolveDeployInstances(ctx, "app3", created.ID, "")
		if len(refs) != 1 || refs[0].Container != "" || refs[0].HostPort != 8080 {
			t.Fatalf("非容器实例应可反查(摘挂升级):%v", refs)
		}
		// 无匹配 → 空。
		if refs, _ := svc.ResolveDeployInstances(ctx, "app1", "", "nope"); len(refs) != 0 {
			t.Fatalf("无匹配应空:%v", refs)
		}
	})
}

// TestGatewayStatusMultiServer 状态快照逐台探测。
func TestGatewayStatusMultiServer(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc, ft := newTestServiceMulti(t, st.DB, "gw1", "gw2")
		ctx := context.Background()
		g, err := svc.GatewayStatus(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !g.Configured || len(g.Servers) != 2 {
			t.Fatalf("应逐台探测:%+v", g)
		}
		if g.Servers[0].ServerID != "gw1" || g.Servers[1].ServerID != "gw2" {
			t.Fatalf("逐台顺序应与配置一致:%+v", g.Servers)
		}
		// 旧平铺字段填第一台(兼容)。
		if g.ServerID != "gw1" {
			t.Fatalf("旧字段应填第一台:%+v", g)
		}
		for _, gw := range []string{"gw1", "gw2"} {
			if !hasCmdOn(ft, gw, "docker", "inspect", "pipewright-nginx", "--format") {
				t.Fatalf("%s 应被探测:%s", gw, joinAllCmds(ft))
			}
		}
	})
}

// TestCreateDomainCertSyncHook:新建基域后 best-effort 拉取既有覆盖证书(免手动刷新);
// 钩子失败不影响建域结果。未注入钩子时建域照常。
func TestCreateDomainCertSyncHook(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc, _ := newTestService(t, st.DB)
		ctx := context.Background()
		setHook := func(h func(context.Context) error) {
			svc.(interface{ SetCertSyncHook(func(context.Context) error) }).SetCertSyncHook(h)
		}

		// 未注入钩子:建域照常。
		if _, err := svc.CreateDomain(ctx, "efg.com"); err != nil {
			t.Fatalf("未注入钩子建域:%v", err)
		}

		calls := 0
		setHook(func(context.Context) error { calls++; return nil })
		if _, err := svc.CreateDomain(ctx, "other.com"); err != nil {
			t.Fatalf("创建基域:%v", err)
		}
		if calls != 1 {
			t.Fatalf("建域应触发一次证书拉取,得 %d", calls)
		}

		// 钩子失败:best-effort,不回滚不报错。
		setHook(func(context.Context) error { return errors.New("vault unavailable") })
		dom, err := svc.CreateDomain(ctx, "third.com")
		if err != nil || dom.BaseDomain != "third.com" {
			t.Fatalf("钩子失败不应影响建域:%v %+v", err, dom)
		}
	})
}

// --- 实例域 ------------------------------------------------------------------

// TestCreateServiceNoAutoInstance 集群模型:创建服务不再自动建实例 #1 —— 实例 =
// (服务器, 宿主端口) 由部署自动注册或人工添加(首个实例就位前渲染 503 维护态)。
func TestCreateServiceNoAutoInstance(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc, ft := newTestService(t, st.DB)
		ctx := context.Background()
		dom, err := svc.CreateDomain(ctx, "efg.com")
		if err != nil {
			t.Fatal(err)
		}
		created, err := svc.CreateService(ctx, CreateServiceInput{
			DomainID: dom.ID, Name: "abc", Protocol: ProtocolHTTP,
			UpstreamKind: UpstreamKindContainer, Upstream: "web", UpstreamPort: 8080,
		})
		if err != nil {
			t.Fatal(err)
		}
		insts, err := svc.ListInstances(ctx, created.ID)
		if err != nil || len(insts) != 0 {
			t.Fatalf("集群模型创建服务不应自动建实例:%v %v", insts, err)
		}
		// 部署托管的 http+container 服务 Upstream 可空(实例行携带容器名)。
		if _, err := svc.CreateService(ctx, CreateServiceInput{
			DomainID: dom.ID, Name: "auto", UpstreamPort: 8080,
		}); err != nil {
			t.Fatalf("http+container 空 Upstream 应合法(部署托管):%v", err)
		}
		// tcp / address 服务仍要求显式上游。
		if _, err := svc.CreateService(ctx, CreateServiceInput{
			DomainID: dom.ID, Name: "db", Protocol: ProtocolTCP,
			UpstreamKind: UpstreamKindContainer, Upstream: "mysql", UpstreamPort: 3306, TCPListenPort: 13306,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.CreateService(ctx, CreateServiceInput{
			DomainID: dom.ID, Name: "badtcp", Protocol: ProtocolTCP, UpstreamPort: 3306, TCPListenPort: 13307,
		}); err != ErrInvalidUpstream {
			t.Fatalf("tcp 无上游应 ErrInvalidUpstream,得 %v", err)
		}
		_ = ft
	})
}

// TestEnsureService 幂等注册(部署节点「一键生成」):不存在 → 创建;存在 → 刷新上游默认;
// 同名不同协议 → 拒绝(人工决断)。
func TestEnsureService(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc, _ := newTestService(t, st.DB)
		ctx := context.Background()
		dom, _ := svc.CreateDomain(ctx, "efg.com")

		a, err := svc.EnsureService(ctx, CreateServiceInput{DomainID: dom.ID, Name: "ABC", UpstreamKind: UpstreamKindContainer, UpstreamPort: 8080})
		if err != nil || a.Name != "abc" {
			t.Fatalf("首保创建:%v %+v", err, a)
		}
		// 再次 ensure(端口变化)→ 复用同一服务行并刷新端口。
		b, err := svc.EnsureService(ctx, CreateServiceInput{DomainID: dom.ID, Name: "abc", UpstreamKind: UpstreamKindContainer, UpstreamPort: 9090})
		if err != nil || b.ID != a.ID || b.UpstreamPort != 9090 {
			t.Fatalf("重复 ensure 应复用并刷新:%v %+v", err, b)
		}
		list, _ := svc.ListServices(ctx)
		if len(list) != 1 {
			t.Fatalf("应只有 1 个服务:%v", list)
		}
		// 同名不同协议 → 拒绝。
		if _, err := svc.EnsureService(ctx, CreateServiceInput{DomainID: dom.ID, Name: "abc", Protocol: ProtocolTCP, Upstream: "x", UpstreamPort: 80, TCPListenPort: 13306}); err != ErrServiceTaken {
			t.Fatalf("协议冲突应拒绝,得 %v", err)
		}
	})
}

// TestEnsureServiceAddressKind 非容器「一键生成」形态:http+address 的 Upstream 可空
// (部署托管,空时渲染维护态);非空须为合法主机地址。tcp 仍强制显式上游。
func TestEnsureServiceAddressKind(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc, _ := newTestService(t, st.DB)
		ctx := context.Background()
		dom, _ := svc.CreateDomain(ctx, "efg.com")

		a, err := svc.EnsureService(ctx, CreateServiceInput{DomainID: dom.ID, Name: "app", UpstreamKind: UpstreamKindAddress, UpstreamPort: 8080})
		if err != nil {
			t.Fatalf("address+空上游应放行(部署托管):%v", err)
		}
		if b, err := svc.EnsureService(ctx, CreateServiceInput{DomainID: dom.ID, Name: "app", UpstreamKind: UpstreamKindAddress, UpstreamPort: 9090}); err != nil || b.ID != a.ID || b.UpstreamPort != 9090 {
			t.Fatalf("重复 ensure 应复用并刷新:%v %+v", err, b)
		}
		// 非空上游须过主机地址校验。
		if _, err := svc.EnsureService(ctx, CreateServiceInput{DomainID: dom.ID, Name: "bad", UpstreamKind: UpstreamKindAddress, Upstream: "bad host!", UpstreamPort: 80}); err != ErrInvalidUpstream {
			t.Fatalf("非法上游应 ErrInvalidUpstream,得 %v", err)
		}
		// tcp 仍强制显式上游。
		if _, err := svc.EnsureService(ctx, CreateServiceInput{DomainID: dom.ID, Name: "db", Protocol: ProtocolTCP, UpstreamKind: UpstreamKindAddress, UpstreamPort: 3306, TCPListenPort: 13306}); err != ErrInvalidUpstream {
			t.Fatalf("tcp 空上游应 ErrInvalidUpstream,得 %v", err)
		}
	})
}

// TestEnsureInstanceDetached 部署期预注册:新行 detached(不进 upstream,不触发 apply);
// 存量行只更新端口/归属,attached 原状保持。
func TestEnsureInstanceDetached(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc, ft := newTestService(t, st.DB)
		ctx := context.Background()
		dom, _ := svc.CreateDomain(ctx, "efg.com")
		created, err := svc.CreateService(ctx, CreateServiceInput{DomainID: dom.ID, Name: "abc", UpstreamPort: 8080})
		if err != nil {
			t.Fatal(err)
		}
		execBefore := len(ft.execCalls)

		// 新行:detached 落库,不触发 apply(无编排命令)。
		i1, err := svc.EnsureInstanceDetached(ctx, created.ID, "app1", "", 8080, 8080)
		if err != nil || i1.Attached {
			t.Fatalf("预注册新行应 detached:%v %+v", err, i1)
		}
		if len(ft.execCalls) != execBefore {
			t.Fatalf("预注册不应触发 apply:%v", ft.execCalls[execBefore:])
		}
		// 挂回后再预注册:attached 原状保持(摘挂流程负责摘),端口更新。
		if err := svc.SetInstanceAttached(ctx, i1.ID, true); err != nil {
			t.Fatal(err)
		}
		i1b, err := svc.EnsureInstanceDetached(ctx, created.ID, "app1", "", 9090, 9090)
		if err != nil || !i1b.Attached || i1b.ID != i1.ID || i1b.Port != 9090 {
			t.Fatalf("存量 attached 行应保持 attached 并刷新端口:%v %+v", err, i1b)
		}
	})
}

// TestResolveInstanceAddrsSkippedCountsAttachedOnly skipped 只统计 attached 实例:
// detached 本就不参与渲染;attached 但遗留行/地址解析失败/容器实例未发布宿主端口才计入。
func TestResolveInstanceAddrsSkippedCountsAttachedOnly(t *testing.T) {
	s := &service{serverAddr: func(_ context.Context, id string) (string, error) {
		if id == "bad" {
			return "", errors.New("unreachable")
		}
		return "10.0.0.1", nil
	}}
	instances := []Instance{
		{ID: "i1", ServerID: "ok", Attached: false},     // detached:不计
		{ID: "i2", ServerID: "", Attached: false},       // detached 遗留:不计
		{ID: "i3", ServerID: "", Attached: true},        // 遗留行:计
		{ID: "i4", ServerID: "bad", Attached: true},     // 地址解析失败:计
		{ID: "i5", ServerID: "ok", Container: "app-1", Attached: true},                // 容器无宿主端口:计
		{ID: "i6", ServerID: "ok", Container: "app-2", HostPort: 20001, Attached: true}, // 正常容器实例:不计
		{ID: "i7", ServerID: "ok", Attached: true},      // 非容器实例(HostPort=0=继承 Port):不计
	}
	addrs, skipped := s.resolveInstanceAddrs(context.Background(), instances)
	if skipped != 3 {
		t.Fatalf("skipped 应 3(i3/i4/i5),得 %d", skipped)
	}
	if addrs["ok"] != "10.0.0.1" {
		t.Fatalf("ok 应解析:%v", addrs)
	}
}

// TestEnsureInstanceAndPrune 部署期 upsert + 认领遗留行 + 同步清理。
func TestEnsureInstanceAndPrune(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc, _ := newTestService(t, st.DB)
		ctx := context.Background()
		dom, _ := svc.CreateDomain(ctx, "efg.com")
		created, err := svc.CreateService(ctx, CreateServiceInput{DomainID: dom.ID, Name: "abc", UpstreamPort: 8080})
		if err != nil {
			t.Fatal(err)
		}

		// 首保插入(attached,host_port 待轮转 Swap 补)。
		i1, err := svc.EnsureInstance(ctx, created.ID, "app1", "abc-c", 8080, 0)
		if err != nil || !i1.Attached {
			t.Fatalf("ensure 插入:%v %+v", err, i1)
		}
		// 重复 ensure:行复用,hostPort<=0 时不覆盖既有 host_port(轮转 Swap 负责更新)。
		i1b, err := svc.EnsureInstance(ctx, created.ID, "app1", "abc-c", 8080, 0)
		if err != nil || i1b.ID != i1.ID {
			t.Fatalf("重复 ensure 应复用行:%v %+v", err, i1b)
		}
		// hostPort>0 时更新(硬切回退路径落实际端口)。
		if _, err := svc.EnsureInstance(ctx, created.ID, "app1", "abc-c", 8080, 20005); err != nil {
			t.Fatalf("ensure 更新端口:%v", err)
		}
		all, _ := svc.ListInstances(ctx, created.ID)
		if len(all) != 1 || all[0].HostPort != 20005 {
			t.Fatalf("应仍 1 行且端口已更新:%+v", all)
		}

		// 认领遗留行(server_id='' 的 0068 前数据):按(服务, 容器名)命中并补齐归属。
		if _, err := st.DB.ExecContext(ctx,
			`INSERT INTO service_reg_instances (id, service_id, server_id, container, port, host_port, attached, created_at, updated_at)
			 VALUES ('legacy-1', ?, '', 'abc-old', 8080, 0, 1, ?, ?)`,
			created.ID, time.Now().UTC().Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
		claimed, err := svc.EnsureInstance(ctx, created.ID, "app2", "abc-old", 8080, 0)
		if err != nil || claimed.ID != "legacy-1" || claimed.ServerID != "app2" {
			t.Fatalf("应认领遗留行:%v %+v", err, claimed)
		}

		// 同步清理:保留 app1,清掉 app2 的实例(含遗留认领行)。
		n, err := svc.PruneInstancesNotIn(ctx, created.ID, []string{"app1"})
		if err != nil || n != 1 {
			t.Fatalf("清理数应 1:%d %v", n, err)
		}
		all, _ = svc.ListInstances(ctx, created.ID)
		if len(all) != 1 || all[0].ServerID != "app1" {
			t.Fatalf("清理后应只剩 app1:%+v", all)
		}
		// 清空(服务器全部移出)→ 全删。
		if n, err := svc.PruneInstancesNotIn(ctx, created.ID, nil); err != nil || n != 1 {
			t.Fatalf("清空应删 1:%d %v", n, err)
		}
	})
}

func TestInstanceLifecycleAndSwap(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc, ft := newTestService(t, st.DB)
		ctx := context.Background()
		dom, _ := svc.CreateDomain(ctx, "efg.com")
		if _, err := svc.UpdateSettings(ctx, SettingsUpdate{ServerID: strPtr("srv1")}); err != nil {
			t.Fatal(err)
		}
		created, err := svc.CreateService(ctx, CreateServiceInput{
			DomainID: dom.ID, Name: "abc", Upstream: "abc-1", UpstreamPort: 8080,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.AddInstance(ctx, created.ID, "srv1", "abc-1", 0, 0); err != nil {
			t.Fatal(err)
		}

		// 加第二个实例(另一台机器;同容器名不同服务器不冲突)。
		inst2, err := svc.AddInstance(ctx, created.ID, "srv2", "abc-2", 0, 0)
		if err != nil || !inst2.Attached {
			t.Fatalf("加实例:%v %+v", err, inst2)
		}
		if _, err := svc.AddInstance(ctx, created.ID, "srv2", "abc-2", 0, 0); err != ErrInstanceTaken {
			t.Fatalf("同服务器重名实例应 ErrInstanceTaken,得 %v", err)
		}
		if _, err := svc.AddInstance(ctx, created.ID, "", "abc-3", 0, 0); err != ErrInvalidInstance {
			t.Fatalf("缺服务器应 ErrInvalidInstance,得 %v", err)
		}
		if _, err := svc.AddInstance(ctx, created.ID, "srv1", "-bad", 0, 0); err != ErrInvalidInstance {
			t.Fatalf("非法容器名应 ErrInvalidInstance,得 %v", err)
		}
		all, _ := svc.ListInstances(ctx, created.ID)
		if len(all) != 2 {
			t.Fatalf("应 2 实例:%v", all)
		}

		// Swap:一次 reload 完成成员替换 + host_port 同步(数 reload 次数)。
		ft.execCalls = nil
		if err := svc.SwapInstance(ctx, created.ID, "srv1", "abc-1", "abc-1-r1a2b3", 20001); err != nil {
			t.Fatalf("swap:%v", err)
		}
		reloadCount := 0
		for _, c := range ft.execCalls {
			if strings.Join(c, " ") == "docker exec pipewright-nginx nginx -s reload" {
				reloadCount++
			}
		}
		if reloadCount != 1 {
			t.Fatalf("Swap 应恰好一次 reload,得 %d:\n%s", reloadCount, joinAllCmds(ft))
		}
		after, _ := svc.ListInstances(ctx, created.ID)
		found := false
		for _, i := range after {
			if i.Container == "abc-1-r1a2b3" {
				found = true
				if i.HostPort != 20001 || i.ServerID != "srv1" {
					t.Fatalf("swap 后应同步宿主端口与归属:%+v", i)
				}
			}
		}
		if !found {
			t.Fatalf("swap 后实例行应指向新容器:%v", after)
		}

		// 摘挂:摘除后实例仍在配置,attached=false。
		if err := svc.SetInstanceAttached(ctx, inst2.ID, false); err != nil {
			t.Fatal(err)
		}
		after, _ = svc.ListInstances(ctx, created.ID)
		for _, i := range after {
			if i.ID == inst2.ID && i.Attached {
				t.Fatalf("实例应已摘除:%+v", i)
			}
		}
		// 删除实例。
		if err := svc.RemoveInstance(ctx, inst2.ID); err != nil {
			t.Fatal(err)
		}
		after, _ = svc.ListInstances(ctx, created.ID)
		if len(after) != 1 {
			t.Fatalf("删后应剩 1 实例:%v", after)
		}
		// 删服务级联清实例。
		if err := svc.DeleteService(ctx, created.ID); err != nil {
			t.Fatal(err)
		}
		if after, _ = svc.ListInstances(ctx, created.ID); len(after) != 0 {
			t.Fatalf("删服务应级联清实例:%v", after)
		}
	})
}

func TestResolveDeployInstances(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc, _ := newTestService(t, st.DB)
		ctx := context.Background()
		dom, _ := svc.CreateDomain(ctx, "efg.com")
		created, err := svc.CreateService(ctx, CreateServiceInput{
			DomainID: dom.ID, Name: "abc", Upstream: "abc-1", UpstreamPort: 8080,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.AddInstance(ctx, created.ID, "gw", "abc-1", 0, 0); err != nil {
			t.Fatal(err)
		}
		inst2, _ := svc.AddInstance(ctx, created.ID, "gw", "abc-2", 9090, 0)

		// 实例容器名精确匹配:返回该服务器上全部 attached 实例(生效端口)。
		refs, err := svc.ResolveDeployInstances(ctx, "gw", "", "abc-1")
		if err != nil || len(refs) != 2 {
			t.Fatalf("应返回 gw 上 2 实例:%v %v", refs, err)
		}
		port2 := 0
		for _, r := range refs {
			if r.Container == "abc-2" {
				port2 = r.Port
			}
		}
		if port2 != 9090 {
			t.Fatalf("实例覆盖端口未生效:%d", port2)
		}
		if refs[0].ServiceName != "abc.efg.com" {
			t.Fatalf("ServiceName 应为 FQDN:%q", refs[0].ServiceName)
		}
		// 其它服务器 → 空(绝不返回别机实例)。
		if refs, _ := svc.ResolveDeployInstances(ctx, "other", "", "abc-1"); len(refs) != 0 {
			t.Fatalf("其它服务器应空:%v", refs)
		}

		// 服务名匹配(惯例 abc → abc-1/abc-2)。
		refs, _ = svc.ResolveDeployInstances(ctx, "gw", "", "abc")
		if len(refs) != 2 {
			t.Fatalf("服务名匹配应返回全部实例:%v", refs)
		}

		// 摘除的实例不参与轮转。
		if err := svc.SetInstanceAttached(ctx, inst2.ID, false); err != nil {
			t.Fatal(err)
		}
		refs, _ = svc.ResolveDeployInstances(ctx, "gw", "", "abc")
		if len(refs) != 1 || refs[0].Container != "abc-1" {
			t.Fatalf("摘除实例不应参与轮转:%v", refs)
		}

		// 无匹配 → 空。
		refs, _ = svc.ResolveDeployInstances(ctx, "gw", "", "nope")
		if len(refs) != 0 {
			t.Fatalf("无匹配应空:%v", refs)
		}
	})
}

// TestApplyClusterRender 集群渲染:apply 不再探活/剔除本机容器,各网关推同一份全量配置,
// upstream 成员 = 服务器地址:宿主端口;遗留行(server_id='')跳过并写入提示注释。
func TestApplyClusterRender(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc, ft := newTestServiceMulti(t, st.DB, "gw1", "gw2")
		ctx := context.Background()
		dom, _ := svc.CreateDomain(ctx, "efg.com")
		created, err := svc.CreateService(ctx, CreateServiceInput{
			DomainID: dom.ID, Name: "abc", Upstream: "abc-1", UpstreamPort: 8080,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.AddInstance(ctx, created.ID, "app1", "abc-1", 0, 20001); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.AddInstance(ctx, created.ID, "app2", "abc-2", 0, 20002); err != nil {
			t.Fatal(err)
		}
		// 遗留行(server_id=''):渲染跳过。
		if _, err := st.DB.ExecContext(ctx,
			`INSERT INTO service_reg_instances (id, service_id, server_id, container, port, host_port, attached, created_at, updated_at)
			 VALUES ('legacy', ?, '', 'abc-old', 8080, 0, 1, ?, ?)`,
			created.ID, time.Now().UTC().Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}

		// 重置计数后单看本轮 apply(此前每次变更都已 best-effort 收敛过)。
		ft.execCalls, ft.uploads, ft.uploadServers, ft.uploadBytes = nil, nil, nil, nil
		if err := svc.Apply(ctx); err != nil {
			t.Fatalf("apply:%v", err)
		}
		var conf string
		for path, content := range ft.uploadBytes {
			if path == "/tmp/pipewright-nginx.conf" {
				conf = content
			}
		}
		// 集群成员:两台网关收到同一份配置,远端实例同样渲染。
		for _, want := range []string{
			"server 10.9.9.9:20001 max_fails=2 fail_timeout=10s;",
			"server 10.9.9.9:20002 max_fails=2 fail_timeout=10s;",
		} {
			if !strings.Contains(conf, want) {
				t.Fatalf("集群成员缺失(%q):\n%s", want, conf)
			}
		}
		if strings.Contains(conf, "abc-old") {
			t.Fatalf("遗留行不应渲染:\n%s", conf)
		}
		if !strings.Contains(conf, "有 1 个实例因遗留数据") {
			t.Fatalf("遗留行应计数提示:\n%s", conf)
		}
		// 不再对实例容器做本机探活/接网(docker inspect <实例容器> / network connect 均不应出现)。
		for _, c := range ft.execCalls {
			joined := strings.Join(c, " ")
			if strings.Contains(joined, "docker network connect") {
				t.Fatalf("http 实例不再接共享网络:\n%s", joinAllCmds(ft))
			}
			if strings.HasPrefix(joined, "docker inspect") && strings.Contains(joined, "abc-") {
				t.Fatalf("不再逐实例探活:\n%s", joinAllCmds(ft))
			}
		}
		// 两台网关各收到一次配置下发。
		pushes := 0
		for i, u := range ft.uploads {
			if u == "/tmp/pipewright-nginx.conf" && ft.uploadServers[i] != "" {
				pushes++
			}
		}
		if pushes != 2 {
			t.Fatalf("两台网关应各推一份配置,得 %d:\n%s", pushes, joinAllCmds(ft))
		}
	})
}

// --- 测试工具 --------------------------------------------------------------

func strPtr(s string) *string { return &s }

func uploadTo(ft *fakeTarget, path string) bool {
	for _, u := range ft.uploads {
		if u == path {
			return true
		}
	}
	return false
}

func joinAllCmds(ft *fakeTarget) string {
	var b strings.Builder
	for _, c := range ft.execCalls {
		b.WriteString(strings.Join(c, " ") + "\n")
	}
	return b.String()
}

// selfSignedCertPEM 生成一张自签名证书(含 SAN)与私钥的 PEM(测试专用)。
func selfSignedCertPEM(t *testing.T, dnsNames []string) (certPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		DNSNames:     dnsNames,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return certPEM, keyPEM
}
