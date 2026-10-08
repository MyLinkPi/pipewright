package registryhub

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/storetest"
)

// fakeCertSource 是可控的 CertSource(证书摘要 + PEM 解密;不触 certmgmt 真库)。
type fakeCertSource struct {
	certs map[string]*CertInfo
	pem   string
}

func (f *fakeCertSource) GetCert(_ context.Context, id string) (*CertInfo, error) {
	c, ok := f.certs[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return c, nil
}

func (f *fakeCertSource) OpenCertPEM(_ context.Context, _ string) (string, string, error) {
	return f.pem, f.pem, nil
}

// failRunner 是恒失败的 LocalRunner(composeBin 双探测都失败 → 确定性 ErrNoLocalDocker,
// 不依赖测试机是否装了 docker)。
type failRunner struct{}

func (failRunner) Run(context.Context, string, []string, string) (string, string, int, error) {
	return "", "", 1, nil
}

func TestDomainCovers(t *testing.T) {
	cases := []struct {
		domains []string
		host    string
		want    bool
	}{
		{[]string{"pipe.mylinkpi.cn"}, "pipe.mylinkpi.cn", true},
		{[]string{"*.mylinkpi.cn"}, "pipe.mylinkpi.cn", true},
		{[]string{"*.mylinkpi.cn"}, "a.b.mylinkpi.cn", false}, // 通配符只盖最左一个标签
		{[]string{"*.mylinkpi.cn"}, "mylinkpi.cn", false},     // 裸域不被通配符覆盖
		{[]string{"other.com"}, "pipe.mylinkpi.cn", false},
		{nil, "pipe.mylinkpi.cn", false},
		{[]string{"*.mylinkpi.cn"}, "", false},
	}
	for _, tc := range cases {
		if got := domainCovers(tc.domains, tc.host); got != tc.want {
			t.Fatalf("domainCovers(%v, %q) = %v, want %v", tc.domains, tc.host, got, tc.want)
		}
	}
}

func TestConfigTLSCertSaveValidation(t *testing.T) {
	good := &fakeCertSource{certs: map[string]*CertInfo{
		"cert-1": {ID: "cert-1", PrimaryDomain: "mylinkpi.cn", Domains: []string{"mylinkpi.cn", "*.mylinkpi.cn"}},
	}}
	cases := []struct {
		name    string
		opts    Options
		in      SaveInput
		wantErr bool
	}{
		{"未选证书合法", Options{Certs: good}, SaveInput{Enabled: true, ExternalAddr: "pipe.mylinkpi.cn"}, false},
		{"证书域名覆盖", Options{Certs: good}, SaveInput{Enabled: true, ExternalAddr: "pipe.mylinkpi.cn", TLSCertID: "cert-1"}, false},
		{"证书不存在", Options{Certs: good}, SaveInput{Enabled: true, ExternalAddr: "pipe.mylinkpi.cn", TLSCertID: "cert-404"}, true},
		{"域名不覆盖", Options{Certs: good}, SaveInput{Enabled: true, ExternalAddr: "other.com", TLSCertID: "cert-1"}, true},
		{"证书能力未装配", Options{}, SaveInput{Enabled: true, ExternalAddr: "pipe.mylinkpi.cn", TLSCertID: "cert-1"}, true},
		{"ID 含空白拒绝", Options{Certs: good}, SaveInput{Enabled: true, ExternalAddr: "pipe.mylinkpi.cn", TLSCertID: "a b"}, true},
		{"ID 超长拒绝", Options{Certs: good}, SaveInput{Enabled: true, ExternalAddr: "pipe.mylinkpi.cn", TLSCertID: strings.Repeat("a", 129)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHub(t, tc.opts)
			_, err := h.Save(context.Background(), tc.in)
			if tc.wantErr && (err == nil || !errors.Is(err, ErrInvalidTLSCert)) {
				t.Fatalf("应 ErrInvalidTLSCert, got %v", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("应合法, got %v", err)
			}
		})
	}
}

func TestConfigTLSCertRoundtrip(t *testing.T) {
	base := t.TempDir()
	certs := &fakeCertSource{certs: map[string]*CertInfo{
		"cert-1": {ID: "cert-1", Domains: []string{"*.mylinkpi.cn"}},
	}}
	db := storetest.OpenDB(t)
	h := New(db, nil, Options{BaseDir: base, Certs: certs})
	cfg, err := h.Save(context.Background(), SaveInput{Enabled: true, ExternalAddr: "pipe.mylinkpi.cn", TLSCertID: "cert-1"})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if cfg.TLSCertID != "cert-1" || !cfg.TLSEnabled() {
		t.Fatalf("TLS 配置不符: %+v", cfg)
	}
	h2 := New(db, nil, Options{BaseDir: base, Certs: certs})
	got, err := h2.Get(context.Background())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.TLSCertID != "cert-1" {
		t.Fatalf("重载 TLS 证书 ID 不符: %+v", got)
	}
	// UsesCert 占用检查。
	if ok, err := h.UsesCert(context.Background(), "cert-1"); err != nil || !ok {
		t.Fatalf("UsesCert 应 true: %v %v", ok, err)
	}
	if ok, err := h.UsesCert(context.Background(), "other"); err != nil || ok {
		t.Fatalf("UsesCert 应 false: %v %v", ok, err)
	}
}

func TestRenderComposeTLS(t *testing.T) {
	base := t.TempDir()
	h := New(storetest.OpenDB(t), nil, Options{BaseDir: base})
	plain := h.renderCompose(&Config{ExternalAddr: "ctrl", ArtifactPort: 5000, CachePort: 5001})
	if strings.Contains(plain, "REGISTRY_HTTP_TLS") || strings.Contains(plain, "/certs") {
		t.Fatalf("明文栈不得含 TLS 配置:\n%s", plain)
	}
	if strings.Count(plain, "volumes:") != 2 {
		t.Fatalf("明文栈每服务一个 volumes 块:\n%s", plain)
	}
	tlsCfg := &Config{ExternalAddr: "ctrl", ArtifactPort: 5000, CachePort: 5001, TLSCertID: "cert-1"}
	got := h.renderCompose(tlsCfg)
	for _, want := range []string{
		"REGISTRY_HTTP_TLS_CERTIFICATE: /certs/fullchain.pem",
		"REGISTRY_HTTP_TLS_KEY: /certs/privkey.pem",
		"- \"" + filepath.Join(base, "certs") + ":/certs:ro\"",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("TLS compose 缺 %q:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "REGISTRY_HTTP_TLS_CERTIFICATE"); n != 2 {
		t.Fatalf("双服务都应挂 TLS, cert env 出现 %d 次:\n%s", n, got)
	}
	// 目录变更收敛依赖的卷行形态必须保留(parseComposeDataDirs)。
	art, cch := parseComposeDataDirs(got)
	if art == "" || cch == "" {
		t.Fatalf("TLS compose 应仍可解析出 data/cache 卷源: %q %q\n%s", art, cch, got)
	}
}

func TestSyncCertPEMs(t *testing.T) {
	base := t.TempDir()
	certs := &fakeCertSource{certs: map[string]*CertInfo{"cert-1": {ID: "cert-1"}}, pem: "---PEM---"}
	h := New(storetest.OpenDB(t), nil, Options{BaseDir: base, Certs: certs})
	ctx := context.Background()

	// TLS on:fullchain 0644、私钥 0600。
	if err := h.syncCertPEMs(ctx, &Config{TLSCertID: "cert-1"}); err != nil {
		t.Fatalf("sync on: %v", err)
	}
	certPath := filepath.Join(base, "certs", certPEMName)
	keyPath := filepath.Join(base, "certs", certKeyName)
	if b, err := os.ReadFile(certPath); err != nil || string(b) != "---PEM---" {
		t.Fatalf("fullchain 落盘不符: %v %q", err, b)
	}
	if runtime.GOOS != "windows" { // Windows 权限位恒 0666,与生产 Linux 语义不同
		if info, err := os.Stat(keyPath); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("私钥权限应 0600: %v", err)
		}
	}

	// TLS off:证书目录整目录清除(私钥不留盘)。
	if err := h.syncCertPEMs(ctx, &Config{}); err != nil {
		t.Fatalf("sync off: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "certs")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("明文栈不得残留证书目录: %v", err)
	}

	// 证书能力未装配 + TLS on → 诚实报错。
	h2 := New(storetest.OpenDB(t), nil, Options{BaseDir: base})
	if err := h2.syncCertPEMs(ctx, &Config{TLSCertID: "cert-1"}); err == nil {
		t.Fatalf("未装配证书能力应报错")
	}
}

func TestMirrorURLAndDaemonJSONTLS(t *testing.T) {
	tlsCfg := &Config{ExternalAddr: "ctrl", ArtifactPort: 5000, CachePort: 5001, TLSCertID: "cert-1"}
	if got := mirrorURL(tlsCfg); got != "https://ctrl:5001" {
		t.Fatalf("TLS mirror url = %q", got)
	}
	got := string(daemonJSONContent(tlsCfg))
	if !strings.Contains(got, "https://ctrl:5001") {
		t.Fatalf("TLS daemon.json 应 https mirror:\n%s", got)
	}
	if strings.Contains(got, "insecure-registries") {
		t.Fatalf("TLS daemon.json 不得含 insecure-registries:\n%s", got)
	}
}

func TestLocalRegistryURLAndProbeClient(t *testing.T) {
	h := New(storetest.OpenDB(t), nil, Options{})
	plain := &Config{}
	if got := h.localRegistryURL(plain, 5000); got != "http://127.0.0.1:5000" {
		t.Fatalf("明文 url = %q", got)
	}
	if h.probeHTTPClient(plain) != h.opts.HTTPClient {
		t.Fatalf("明文栈应复用共享客户端")
	}
	tlsCfg := &Config{TLSCertID: "c"}
	if got := h.localRegistryURL(tlsCfg, 5001); got != "https://127.0.0.1:5001" {
		t.Fatalf("TLS url = %q", got)
	}
	c := h.probeHTTPClient(tlsCfg)
	if c == h.opts.HTTPClient || c.Transport == nil {
		t.Fatalf("TLS 栈应为独立客户端且带 Transport")
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr.TLSClientConfig == nil || !tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("TLS 探活客户端应 InsecureSkipVerify")
	}
}

func TestRedeployCertLinkage(t *testing.T) {
	certs := &fakeCertSource{certs: map[string]*CertInfo{
		"cert-1": {ID: "cert-1", Domains: []string{"pipe.mylinkpi.cn"}},
	}, pem: "---"}
	h := newTestHub(t, Options{Certs: certs, Runner: failRunner{}})
	ctx := context.Background()
	// 未引用该证书 → no-op(不触部署)。
	if err := h.RedeployCert(ctx, "cert-1"); err != nil {
		t.Fatalf("未引用应 no-op: %v", err)
	}
	// 引用中:保存 TLS 配置后联动重部署 —— 无 docker 环境,部署层诚实报错(联动链路本身走通)。
	if _, err := h.Save(ctx, SaveInput{Enabled: true, ExternalAddr: "pipe.mylinkpi.cn", TLSCertID: "cert-1"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	err := h.RedeployCert(ctx, "cert-1")
	if err == nil || !errors.Is(err, ErrNoLocalDocker) {
		t.Fatalf("引用中应触发重部署(无 docker → ErrNoLocalDocker), got %v", err)
	}
}
