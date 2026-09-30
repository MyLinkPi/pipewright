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
	out := renderNginxConf(nil, nil, nil)
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
	inst := Instance{ID: "i1", ServiceID: "s1", Container: "web", Port: 8080, Attached: true}
	out := renderNginxConf([]Domain{dom}, []RegisteredService{svc}, []Instance{inst})

	mustContain := []string{
		"server_name abc.efg.com;",
		"listen 443 ssl;",
		"ssl_certificate /etc/pipewright/certs/efg.com/fullchain.pem;",
		"ssl_certificate_key /etc/pipewright/certs/efg.com/privkey.pem;",
		"upstream pw_abc {",
		"server web:8080 max_fails=2 fail_timeout=10s;",
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
	// 故意乱序输入:实例按容器名升序渲染,且 detached 实例不进池。
	insts := []Instance{
		{ID: "i2", ServiceID: "s1", Container: "abc-2", Attached: true},
		{ID: "i1", ServiceID: "s1", Container: "abc-1", Port: 9090, Attached: true},
		{ID: "i3", ServiceID: "s1", Container: "abc-3", Attached: false},
	}
	out := renderNginxConf([]Domain{dom}, []RegisteredService{svc}, insts)
	if !strings.Contains(out, "upstream pw_abc {") {
		t.Fatalf("应渲染 upstream 块:\n%s", out)
	}
	// abc-1 覆盖端口 9090;abc-2 继承服务端口 8080;abc-3 已摘除不出现。
	for _, want := range []string{
		"server abc-1:9090 max_fails=2 fail_timeout=10s;",
		"server abc-2:8080 max_fails=2 fail_timeout=10s;",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("实例成员不符(缺 %q):\n%s", want, out)
		}
	}
	if strings.Contains(out, "abc-3") {
		t.Fatalf("摘除实例不应渲染:\n%s", out)
	}
	i1 := strings.Index(out, "server abc-1:")
	i2 := strings.Index(out, "server abc-2:")
	if i1 < 0 || i2 < 0 || i1 > i2 {
		t.Fatalf("实例应按容器名升序:\n%s", out)
	}
}

func TestRenderNginxConfAllInstancesDetached503(t *testing.T) {
	dom := Domain{ID: "d1", BaseDomain: "efg.com", HasCert: false}
	svc := RegisteredService{ID: "s1", Name: "abc", DomainID: "d1", Protocol: ProtocolHTTP,
		UpstreamKind: UpstreamKindContainer, Upstream: "abc-1", UpstreamPort: 8080, Enabled: true}
	out := renderNginxConf([]Domain{dom}, []RegisteredService{svc}, nil)
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

func TestRenderNginxConfHTTPOnlyNoCert(t *testing.T) {
	dom := Domain{ID: "d1", BaseDomain: "efg.com", HasCert: false}
	svc := RegisteredService{Name: "abc", DomainID: "d1", Protocol: ProtocolHTTP,
		UpstreamKind: UpstreamKindAddress, Upstream: "10.0.0.5", UpstreamPort: 3000, Enabled: true}
	out := renderNginxConf([]Domain{dom}, []RegisteredService{svc}, nil)
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
	out := renderNginxConf([]Domain{dom}, []RegisteredService{svc}, nil)
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
		{ID: "i1", ServiceID: "s-alpha", Container: "alphac", Attached: true},
		{ID: "i2", ServiceID: "s-zeta", Container: "zetac", Attached: true},
	}
	services := []RegisteredService{mk("zeta", "d2"), mk("alpha", "d1")}
	a := renderNginxConf([]Domain{dom1, dom2}, services, insts)
	// 输入顺序无关:打乱后重渲染结果应逐字节一致。
	services2 := []RegisteredService{mk("alpha", "d1"), mk("zeta", "d2")}
	insts2 := []Instance{insts[1], insts[0]}
	b := renderNginxConf([]Domain{dom2, dom1}, services2, insts2)
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
	out := renderNginxConf([]Domain{dom}, []RegisteredService{svc}, nil)
	if strings.Contains(out, "abc.efg.com") {
		t.Fatalf("禁用服务不应渲染:\n%s", out)
	}
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
	execCalls   [][]string
	uploads     []string
	uploadBytes map[string]string
	resultFor   func(cmd []string) *target.ExecResult
}

func (f *fakeTarget) Exec(_ context.Context, _ string, cmd []string) (*target.ExecResult, error) {
	f.execCalls = append(f.execCalls, cmd)
	if f.resultFor != nil {
		if r := f.resultFor(cmd); r != nil {
			return r, nil
		}
	}
	return &target.ExecResult{ExitCode: 0}, nil
}

func (f *fakeTarget) Upload(_ context.Context, _ string, content io.Reader, remotePath string) error {
	f.uploads = append(f.uploads, remotePath)
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

func newTestService(t *testing.T, db *sql.DB) (Service, *fakeTarget) {
	t.Helper()
	ft := &fakeTarget{}
	return New(db, ft, fakeSealer{}), ft
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
		if hasCmd(ft, "docker", "cp", "/tmp/pipewright-nginx.conf") {
			t.Fatalf("校验失败不应覆盖正式配置:\n%s", joinAllCmds(ft))
		}
		// 服务应已回滚删除。
		list, _ := svc.ListServices(ctx)
		if len(list) != 0 {
			t.Fatalf("编排失败的服务应回滚:%v", list)
		}
	})
}

func TestUploadTokenLifecycle(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc, _ := newTestService(t, st.DB)
		ctx := context.Background()
		if svc.VerifyUploadToken("anything") {
			t.Fatal("未生成 token 前不应验证通过")
		}
		token, cfg, err := svc.GenerateUploadToken(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.HasUploadToken || token == "" {
			t.Fatalf("token 状态不符:%+v %q", cfg, token)
		}
		if !svc.VerifyUploadToken(token) {
			t.Fatal("正确 token 应验证通过")
		}
		if svc.VerifyUploadToken(token + "x") {
			t.Fatal("错误 token 不应通过")
		}
		if svc.VerifyUploadToken(strings.ToUpper(token)) {
			t.Fatal("token 大小写敏感")
		}
		if err := svc.RevokeUploadToken(ctx); err != nil {
			t.Fatal(err)
		}
		if svc.VerifyUploadToken(token) {
			t.Fatal("撤销后不应通过")
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

// --- 实例域 ------------------------------------------------------------------

func TestCreateServiceAutoInstance(t *testing.T) {
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
		if err != nil || len(insts) != 1 {
			t.Fatalf("http+container 服务应自动建实例 #1:%v %v", insts, err)
		}
		if insts[0].Container != "web" || !insts[0].Attached {
			t.Fatalf("实例 #1 应继承服务上游:%+v", insts[0])
		}
		// tcp / address 服务不建实例。
		tcp, err := svc.CreateService(ctx, CreateServiceInput{
			DomainID: dom.ID, Name: "db", Protocol: ProtocolTCP,
			UpstreamKind: UpstreamKindContainer, Upstream: "mysql", UpstreamPort: 3306, TCPListenPort: 13306,
		})
		if err != nil {
			t.Fatal(err)
		}
		if insts, _ := svc.ListInstances(ctx, tcp.ID); len(insts) != 0 {
			t.Fatalf("tcp 服务不应有实例:%v", insts)
		}
		_ = ft
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

		// 加第二个实例。
		inst2, err := svc.AddInstance(ctx, created.ID, "abc-2", 0)
		if err != nil || !inst2.Attached {
			t.Fatalf("加实例:%v %+v", err, inst2)
		}
		if _, err := svc.AddInstance(ctx, created.ID, "abc-2", 0); err != ErrInstanceTaken {
			t.Fatalf("重名实例应 ErrInstanceTaken,得 %v", err)
		}
		if _, err := svc.AddInstance(ctx, created.ID, "-bad", 0); err != ErrInvalidInstance {
			t.Fatalf("非法容器名应 ErrInvalidInstance,得 %v", err)
		}
		all, _ := svc.ListInstances(ctx, created.ID)
		if len(all) != 2 {
			t.Fatalf("应 2 实例:%v", all)
		}

		// Swap:一次 reload 完成成员替换(数 reload 次数)。
		ft.execCalls = nil
		if err := svc.SwapInstance(ctx, created.ID, "abc-1", "abc-1-r1a2b3"); err != nil {
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
		inst2, _ := svc.AddInstance(ctx, created.ID, "abc-2", 9090)

		// 网关未配置 → 空。
		refs, err := svc.ResolveDeployInstances(ctx, "srv1", "abc-1")
		if err != nil || len(refs) != 0 {
			t.Fatalf("未配网关应空:%v %v", refs, err)
		}
		// 配置网关(其它 serverID)→ 空。
		if _, err := svc.UpdateSettings(ctx, SettingsUpdate{ServerID: strPtr("gw")}); err != nil {
			t.Fatal(err)
		}
		refs, _ = svc.ResolveDeployInstances(ctx, "other", "abc-1")
		if len(refs) != 0 {
			t.Fatalf("非网关主机应空:%v", refs)
		}

		// 实例容器名精确匹配:返回该服务全部 attached 实例(生效端口)。
		refs, err = svc.ResolveDeployInstances(ctx, "gw", "abc-1")
		if err != nil || len(refs) != 2 {
			t.Fatalf("应返回 2 实例:%v %v", refs, err)
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

		// 服务名匹配(惯例 abc → abc-1/abc-2)。
		refs, _ = svc.ResolveDeployInstances(ctx, "gw", "abc")
		if len(refs) != 2 {
			t.Fatalf("服务名匹配应返回全部实例:%v", refs)
		}

		// 摘除的实例不参与轮转。
		if err := svc.SetInstanceAttached(ctx, inst2.ID, false); err != nil {
			t.Fatal(err)
		}
		refs, _ = svc.ResolveDeployInstances(ctx, "gw", "abc")
		if len(refs) != 1 || refs[0].Container != "abc-1" {
			t.Fatalf("摘除实例不应参与轮转:%v", refs)
		}

		// 无匹配 → 空。
		refs, _ = svc.ResolveDeployInstances(ctx, "gw", "nope")
		if len(refs) != 0 {
			t.Fatalf("无匹配应空:%v", refs)
		}
	})
}

func TestApplyPrunesDeadInstances(t *testing.T) {
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
		if _, err := svc.AddInstance(ctx, created.ID, "abc-2", 0); err != nil {
			t.Fatal(err)
		}

		// abc-2 容器已消失(inspect 非零)→ 渲染剔除,其余命令正常走完。
		ft.resultFor = func(cmd []string) *target.ExecResult {
			if len(cmd) >= 4 && cmd[0] == "docker" && cmd[1] == "inspect" && cmd[2] == "--format" && cmd[4] == "abc-2" {
				return &target.ExecResult{ExitCode: 1, Stderr: "Error: No such object"}
			}
			return nil
		}
		if err := svc.Apply(ctx); err != nil {
			t.Fatalf("死实例应被剔除而非卡死 apply:%v", err)
		}
		var conf string
		for path, content := range ft.uploadBytes {
			if path == "/tmp/pipewright-nginx.conf" {
				conf = content
			}
		}
		if !strings.Contains(conf, "server abc-1:8080") {
			t.Fatalf("存活实例应渲染:\n%s", conf)
		}
		if strings.Contains(conf, "abc-2") {
			t.Fatalf("死实例应剔除:\n%s", conf)
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
