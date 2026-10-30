package aptcacherng

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeCerts 是 CertSource 的测试 fake:cert-1 覆盖 *.example.com(cert-1 也覆盖裸域)。
type fakeCerts struct{ openErr error }

func (f fakeCerts) GetCert(_ context.Context, id string) (*CertInfo, error) {
	if id == "cert-1" {
		return &CertInfo{ID: id, PrimaryDomain: "example.com",
			Domains: []string{"example.com", "*.example.com"}, Status: "issued",
			NotAfter: time.Now().AddDate(0, 0, 90)}, nil
	}
	return nil, errors.New("not found")
}

func (f fakeCerts) OpenCertPEM(_ context.Context, _ string) (string, string, error) {
	if f.openErr != nil {
		return "", "", f.openErr
	}
	return "---CERT---", "---KEY---", nil
}

func TestNormalizeConfigDefaults(t *testing.T) {
	cfg, err := NormalizeConfig(SaveInput{}, "/base")
	if err != nil {
		t.Fatalf("零值应可归一: %v", err)
	}
	if cfg.Port != DefaultPort || cfg.DataDir != "/base/cache" || cfg.TLSCertID != "" || cfg.TLSEnabled() {
		t.Fatalf("默认值不符: %+v", cfg)
	}
}

func TestNormalizeConfigRejects(t *testing.T) {
	cases := []struct {
		name string
		in   SaveInput
		want error
	}{
		{"enabled 缺外部地址", SaveInput{Enabled: true}, ErrInvalidAddr},
		{"地址带 scheme", SaveInput{ExternalAddr: "http://x"}, ErrInvalidAddr},
		{"地址带端口", SaveInput{ExternalAddr: "1.2.3.4:3142"}, ErrInvalidAddr},
		{"端口越界", SaveInput{Port: 70000}, ErrInvalidPort},
		{"缓存目录相对路径", SaveInput{DataDir: "cache"}, ErrInvalidDataDir},
		{"缓存目录根路径", SaveInput{DataDir: "/"}, ErrInvalidDataDir},
		{"缓存目录含控制字符", SaveInput{DataDir: "/a\nb"}, ErrInvalidDataDir},
		{"缓存目录罩住栈基目录", SaveInput{DataDir: "/var/lib/pipewright"}, ErrInvalidDataDir},
		{"证书 ID 带空白", SaveInput{TLSCertID: "cert 1"}, ErrInvalidTLSCert},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NormalizeConfig(tc.in, "/var/lib/pipewright/apt-cacher-ng")
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
}

func TestNormalizeConfigCleanPaths(t *testing.T) {
	cfg, err := NormalizeConfig(SaveInput{DataDir: "/data/apt-cache/nested/.."}, "/base")
	if err != nil {
		t.Fatalf("合法目录应归一: %v", err)
	}
	if cfg.DataDir != "/data/apt-cache" {
		t.Fatalf("应 Clean 路径: %q", cfg.DataDir)
	}
}

func TestConfigAddrAndTLSScheme(t *testing.T) {
	plain := &Config{ExternalAddr: "ctrl", Port: 3142}
	if plain.Addr() != "ctrl:3142" || plain.TLSEnabled() {
		t.Fatalf("明文态不符: %+v", plain)
	}
	if got := proxyURL(plain); got != "http://ctrl:3142" {
		t.Fatalf("明文 proxyURL = %q", got)
	}
	tlsCfg := &Config{ExternalAddr: "cache.example.com", Port: 3142, TLSCertID: "cert-1"}
	if !tlsCfg.TLSEnabled() {
		t.Fatalf("选了证书应 TLS")
	}
	if got := proxyURL(tlsCfg); got != "https://cache.example.com:3142" {
		t.Fatalf("TLS proxyURL = %q", got)
	}
	// 裸 IPv6 兜底方括号。
	if got := (&Config{ExternalAddr: "fe80::1", Port: 3142}).Addr(); !strings.HasPrefix(got, "[fe80::1]:") {
		t.Fatalf("IPv6 地址应加方括号: %q", got)
	}
}

func TestSaveValidation(t *testing.T) {
	db := storetestOpen(t)
	s := &configService{db: db, baseDir: t.TempDir(), certs: fakeCerts{}}
	ctx := context.Background()

	// enabled 但地址非法 → 拒绝,不落库。
	if _, err := s.Save(ctx, SaveInput{Enabled: true, ExternalAddr: "http://bad"}); !errors.Is(err, ErrInvalidAddr) {
		t.Fatalf("应 ErrInvalidAddr, got %v", err)
	}
	// 证书不存在 → ErrInvalidTLSCert。
	if _, err := s.Save(ctx, SaveInput{Enabled: true, ExternalAddr: "x.example.com", TLSCertID: "cert-404"}); !errors.Is(err, ErrInvalidTLSCert) {
		t.Fatalf("证书不存在应 ErrInvalidTLSCert, got %v", err)
	}
	// 证书 SAN 不覆盖 host(裸 IP) → ErrInvalidTLSCert。
	if _, err := s.Save(ctx, SaveInput{Enabled: true, ExternalAddr: "10.0.0.8", TLSCertID: "cert-1"}); !errors.Is(err, ErrInvalidTLSCert) {
		t.Fatalf("SAN 未覆盖应 ErrInvalidTLSCert, got %v", err)
	}
	// 合法 TLS 保存 → 回读一致。
	cfg, err := s.Save(ctx, SaveInput{Enabled: true, ExternalAddr: "apt.example.com", TLSCertID: "cert-1", Port: 3150})
	if err != nil {
		t.Fatalf("合法保存应成功: %v", err)
	}
	if cfg.TLSCertID != "cert-1" || cfg.Port != 3150 || cfg.UpdatedAt == nil {
		t.Fatalf("回读不符: %+v", cfg)
	}
	// 单例语义:再保存覆盖而非新增。
	if _, err := s.Save(ctx, SaveInput{Enabled: false, ExternalAddr: "apt.example.com"}); err != nil {
		t.Fatalf("二次保存: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(1) FROM apt_cacher_config`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("应恒为单例行: n=%d err=%v", n, err)
	}
	// 只写库:落库的配置不影响机器(此处仅验证未启用态可正常读取)。
	got, err := s.Get(ctx)
	if err != nil || got.Enabled {
		t.Fatalf("应读到未启用态: %+v err=%v", got, err)
	}
}

func TestDomainCovers(t *testing.T) {
	if !domainCovers([]string{"*.a.com"}, "x.a.com") {
		t.Fatalf("通配符应覆盖一级子域")
	}
	if domainCovers([]string{"*.a.com"}, "x.y.a.com") || domainCovers([]string{"*.a.com"}, "a.com") {
		t.Fatalf("通配符不应覆盖多级子域或裸域")
	}
	if !domainCovers([]string{"A.Example.COM"}, "a.example.com") {
		t.Fatalf("应大小写不敏感")
	}
}
