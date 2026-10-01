package certmgmt

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/storetest"
)

// ---------- 脚本构造(golden) ----------

// TestBuildRunScriptGolden 锁定签发/续期脚本的确定性输出:mkdir → export 凭据 → acme.sh 主操作 →
// install-cert 逐基域。凭据含单引号时必须安全转义。
func TestBuildRunScriptGolden(t *testing.T) {
	c := &Certificate{
		PrimaryDomain: "*.efg.com",
		Domains:       []string{"*.efg.com", "efg.com"},
		CA:            CALetsEncrypt,
		KeyType:       KeyTypeEC256,
	}
	env := [][2]string{{"CF_Token", "s3cr'et"}, {"CF_AccountId", "acct"}}
	got := buildRunScript(issueOp, c, "dns_cf", env, []string{"efg.com", "aaa.com"})
	want := `#!/bin/sh
set -e
mkdir -p '/etc/pipewright/certs/aaa.com'
mkdir -p '/etc/pipewright/certs/efg.com'
export CF_Token='s3cr'\''et'
export CF_AccountId='acct'
acme.sh --issue --dns dns_cf -d '*.efg.com' -d 'efg.com' --keylength ec-256 --server 'https://acme-v02.api.letsencrypt.org/directory'
acme.sh --install-cert -d '*.efg.com' --fullchain-file '/etc/pipewright/certs/aaa.com/fullchain.pem' --key-file '/etc/pipewright/certs/aaa.com/privkey.pem'
acme.sh --install-cert -d '*.efg.com' --fullchain-file '/etc/pipewright/certs/efg.com/fullchain.pem' --key-file '/etc/pipewright/certs/efg.com/privkey.pem'
`
	if got != want {
		t.Fatalf("issue 脚本不符:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}

	gotRenew := buildRunScript(renewOp, c, "dns_ali", [][2]string{{"Ali_Key", "k"}, {"Ali_Secret", "v"}}, nil)
	wantRenew := `#!/bin/sh
set -e
export Ali_Key='k'
export Ali_Secret='v'
acme.sh --renew -d '*.efg.com' --force
`
	if gotRenew != wantRenew {
		t.Fatalf("renew 脚本不符:\n--- got ---\n%s\n--- want ---\n%s", gotRenew, wantRenew)
	}
}

// TestShQuote 验证 POSIX 单引号转义对任意字节安全。
func TestShQuote(t *testing.T) {
	cases := map[string]string{
		"plain":      `'plain'`,
		"with'quote": `'with'\''quote'`,
		"a b;c":      `'a b;c'`,
		"$x`y":       "'$x`y'",
	}
	for in, want := range cases {
		if got := shQuote(in); got != want {
			t.Fatalf("shQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------- 枚举映射 ----------

func TestDnsAPINameAndEnv(t *testing.T) {
	if dnsAPIName("cloudflare") != "dns_cf" || dnsAPIName("dnspod") != "dns_dp" || dnsAPIName("alidns") != "dns_ali" {
		t.Fatalf("dnsAPIName 映射错误")
	}
	if dnsAPIName("whatever") != "" {
		t.Fatalf("未知类型应返回空串")
	}
	env := dnsEnvFor("dnspod", "my-id", "my-key")
	if len(env) != 2 || env[0] != [2]string{"DP_Id", "my-id"} || env[1] != [2]string{"DP_Key", "my-key"} {
		t.Fatalf("dnspod env 映射错误: %v", env)
	}
}

// ---------- 域名校验 / 覆盖判定 ----------

func TestValidateDomainName(t *testing.T) {
	for _, ok := range []string{"efg.com", "a.efg.com", "*.efg.com", "a-b.example.co"} {
		if err := validateDomainName(ok); err != nil {
			t.Fatalf("%q 应合法: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "*efg.com", "a.*.com", "efg", "e f.com", "efg.com:80", strings.Repeat("x", 300)} {
		if err := validateDomainName(bad); err == nil {
			t.Fatalf("%q 应非法", bad)
		}
	}
}

func TestDomainCoversAny(t *testing.T) {
	sans := []string{"*.efg.com", "efg.com", "other.io"}
	for _, covered := range []string{"efg.com"} {
		if !domainCoversAny(sans, covered) {
			t.Fatalf("基域 %s 应被覆盖", covered)
		}
	}
	// 部分覆盖(SAN 是子域而非基域/通配符)不认:宁缺毋滥。
	if domainCoversAny([]string{"app.efg.com"}, "efg.com") {
		t.Fatalf("子域 SAN 不应覆盖基域")
	}
	if domainCoversAny(sans, "another.com") {
		t.Fatalf("无关基域不应被覆盖")
	}
}

// ---------- normalizeCreate ----------

func TestNormalizeCreate(t *testing.T) {
	in := normalizeCreate(CreateInput{
		PrimaryDomain: "  EFG.COM ",
		Domains:       []string{"efg.com", " *.EFG.com ", "", "www.efg.com", "www.efg.com"},
	})
	if in.PrimaryDomain != "efg.com" {
		t.Fatalf("主域归一错误: %q", in.PrimaryDomain)
	}
	if len(in.Domains) != 2 || in.Domains[0] != "*.efg.com" || in.Domains[1] != "www.efg.com" {
		t.Fatalf("附加域应去重去主域去空: %v", in.Domains)
	}
	if in.CA != CALetsEncrypt || in.KeyType != KeyTypeEC256 {
		t.Fatalf("CA/密钥类型默认值错误: %q %q", in.CA, in.KeyType)
	}
}

// ---------- fake 依赖 ----------

// fakeResolver 是 CredentialsResolver 的测试替身(单提供商 cloudflare,托管 efg.com 根区)。
type fakeResolver struct {
	zones    []string
	secret   string
	provider string // ProviderType 返回值
}

func (f *fakeResolver) Resolve(_ context.Context, _ string) (string, string, string, bool, error) {
	return f.provider, "", f.secret, true, nil
}
func (f *fakeResolver) ProviderType(_ context.Context, _ string) (string, bool, error) {
	return f.provider, true, nil
}
func (f *fakeResolver) ProviderZones(_ context.Context, _ string) ([]string, bool, error) {
	return f.zones, true, nil
}

// fakeSink 是 CertSink 的测试替身。
type fakeSink struct {
	bases    []BaseDomain
	uploaded []string
	cleared  []string
	current  map[string]string
}

func (f *fakeSink) ListBaseDomains(_ context.Context) ([]BaseDomain, error) { return f.bases, nil }
func (f *fakeSink) UploadCert(_ context.Context, domainID, _, _ string) error {
	f.uploaded = append(f.uploaded, domainID)
	return nil
}
func (f *fakeSink) CurrentCertPEM(_ context.Context, domainID string) (string, bool, error) {
	v, ok := f.current[domainID]
	return v, ok, nil
}
func (f *fakeSink) ClearCert(_ context.Context, domainID string) error {
	f.cleared = append(f.cleared, domainID)
	return nil
}

// ---------- validateCreate / 覆盖校验 ----------

func TestValidateCreateZoneCoverage(t *testing.T) {
	s := &service{dns: &fakeResolver{zones: []string{"efg.com"}, provider: "cloudflare"}}
	ctx := context.Background()

	ok := CreateInput{PrimaryDomain: "*.efg.com", Domains: []string{"efg.com"}, DNSProviderID: "p1"}
	if err := s.validateCreate(ctx, ok); err != nil {
		t.Fatalf("通配符+裸域应通过: %v", err)
	}

	// 域名不在根区下 → ErrZoneUncovered。
	bad := CreateInput{PrimaryDomain: "other.io", DNSProviderID: "p1"}
	if err := s.validateCreate(ctx, bad); err == nil {
		t.Fatalf("未覆盖域应被拒")
	}

	// 无提供商 → ErrNoDNSProvider;未知类型 → ErrInvalidDNSProvider。
	if err := s.validateCreate(ctx, CreateInput{PrimaryDomain: "efg.com"}); err != ErrNoDNSProvider {
		t.Fatalf("无提供商应 ErrNoDNSProvider, got %v", err)
	}
	s2 := &service{dns: &fakeResolver{zones: []string{"efg.com"}, provider: "hetzner"}}
	if err := s2.validateCreate(ctx, CreateInput{PrimaryDomain: "efg.com", DNSProviderID: "p1"}); err != ErrInvalidDNSProvider {
		t.Fatalf("未知提供商类型应 ErrInvalidDNSProvider, got %v", err)
	}

	// 枚举校验。
	s3 := &service{dns: &fakeResolver{zones: []string{"efg.com"}, provider: "cloudflare"}}
	if err := s3.validateCreate(ctx, CreateInput{PrimaryDomain: "efg.com", DNSProviderID: "p", CA: "bogus"}); err != ErrInvalidCA {
		t.Fatalf("非法 CA 应被拒, got %v", err)
	}
	if err := s3.validateCreate(ctx, CreateInput{PrimaryDomain: "efg.com", DNSProviderID: "p", KeyType: "rsa-512"}); err != ErrInvalidKeyType {
		t.Fatalf("非法密钥类型应被拒, got %v", err)
	}
}

// ---------- coveredBaseDomains(经 fakeSink) ----------

func TestCoveredBaseDomains(t *testing.T) {
	s := &service{sink: &fakeSink{bases: []BaseDomain{
		{ID: "d1", BaseDomain: "efg.com"},
		{ID: "d2", BaseDomain: "other.io"},
	}}}
	got, err := s.coveredBaseDomains(context.Background(), []string{"*.efg.com", "efg.com"})
	if err != nil {
		t.Fatalf("coveredBaseDomains: %v", err)
	}
	if len(got) != 1 || got[0].ID != "d1" {
		t.Fatalf("应只覆盖 efg.com: %+v", got)
	}
}

// ---------- SweepOnce 窗口判定 ----------

func TestSweepOnceWindowDB(t *testing.T) {
	db := storetest.OpenDB(t)
	ctx := context.Background()
	svc := New(db, nil, nil).(*service)
	st := svc.store

	mk := func(id, primary string, mutate func(*Certificate)) {
		c := &Certificate{
			ID: id, PrimaryDomain: primary, Domains: []string{primary},
			Source: SourceACME, CA: CALetsEncrypt, Validation: ValidationDNS,
			DNSProviderID: "p", KeyType: KeyTypeEC256, AutoRenew: true,
			Status: StatusIssued, NotAfter: time.Now().Add(20 * 24 * time.Hour),
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if mutate != nil {
			mutate(c)
		}
		if err := st.insert(ctx, c); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	mk("c-due", "due.com", nil)                   // 应触发
	mk("c-far", "far.com", func(c *Certificate) { // 窗口外:不触发
		c.NotAfter = time.Now().Add(40 * 24 * time.Hour)
	})
	mk("c-manual", "manual.com", func(c *Certificate) { // manual:不触发
		c.Source = SourceManual
	})
	mk("c-off", "off.com", func(c *Certificate) { c.AutoRenew = false }) // 关自动续期:不触发
	mk("c-pending", "pending.com", func(c *Certificate) { c.Status = StatusPending })
	mk("c-backoff", "backoff.com", func(c *Certificate) { // 失败退避 24h 内:不触发
		c.Status = StatusFailed
		c.LastAttemptAt = time.Now().Add(-2 * time.Hour)
	})
	mk("c-retry", "retry.com", func(c *Certificate) { // 失败但距上次尝试 >24h:触发
		c.Status = StatusFailed
		c.LastAttemptAt = time.Now().Add(-25 * time.Hour)
	})

	// gw 未注入:触发的续期 goroutine 会立即失败并回写(vault/gateway 缺失)。
	n, err := svc.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if n != 2 {
		t.Fatalf("应触发 2 张(due + retry), got %d", n)
	}
	// 等异步 goroutine 落定(markFailed 回写),避免测试结束后写已关闭的库。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		due, e1 := st.get(ctx, "c-due")
		retry, e2 := st.get(ctx, "c-retry")
		if e1 == nil && e2 == nil && due.Status == StatusFailed && retry.Status == StatusFailed {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ---------- 平台 HTTPS 联动 ----------

// passSealer 是明文透传的假保险库(仅测流转)。
type passSealer struct{}

func (passSealer) SealSecret(p []byte) ([]byte, error) { return append([]byte("sealed:"), p...), nil }
func (passSealer) OpenSecret(s []byte) ([]byte, error) {
	return []byte(strings.TrimPrefix(string(s), "sealed:")), nil
}

// fakePlatformHTTPS 记录平台 HTTPS 联动调用(占用/重下发)。
type fakePlatformHTTPS struct {
	used      map[string]bool
	redeploys []string
}

func (f *fakePlatformHTTPS) UsesCert(_ context.Context, id string) (bool, error) {
	return f.used[id], nil
}
func (f *fakePlatformHTTPS) RedeployCert(_ context.Context, id string) error {
	f.redeploys = append(f.redeploys, id)
	return nil
}

// TestDeleteBlockedByPlatformHTTPS:被平台 HTTPS 占用的证书删除被拦截(ErrCertInUse);
// 未占用照常删除。phttps 未注入(nil)时不拦截。
func TestDeleteBlockedByPlatformHTTPS(t *testing.T) {
	db := storetest.OpenDB(t)
	ctx := context.Background()
	svc := New(db, nil, passSealer{}).(*service)
	mk := func(id, primary string) {
		c := &Certificate{
			ID: id, PrimaryDomain: primary, Domains: []string{primary},
			Source: SourceACME, CA: CALetsEncrypt, Validation: ValidationDNS,
			DNSProviderID: "p", KeyType: KeyTypeEC256, AutoRenew: true,
			Status: StatusIssued, NotAfter: time.Now().Add(20 * 24 * time.Hour),
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if err := svc.store.insert(ctx, c); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	mk("c-used", "used.com")
	mk("c-free", "free.com")

	ph := &fakePlatformHTTPS{used: map[string]bool{"c-used": true}}
	svc.SetPlatformHTTPS(ph)

	if err := svc.Delete(ctx, "c-used"); err == nil || !strings.Contains(err.Error(), "平台 HTTPS") {
		t.Fatalf("占用证书删除应被拦截,得 %v", err)
	}
	if err := svc.Delete(ctx, "c-free"); err != nil {
		t.Fatalf("未占用证书应可删除:%v", err)
	}

	// 未注入联动器 → 不拦截。
	svc2 := New(db, nil, passSealer{}).(*service)
	mk("c-2", "second.com")
	if err := svc2.Delete(ctx, "c-2"); err != nil {
		t.Fatalf("未注入联动器时不应拦截:%v", err)
	}
}

// TestOpenCertPEM:进程内解密返回证书/私钥明文;缺失 PEM / 不存在 → 报错。
func TestOpenCertPEM(t *testing.T) {
	db := storetest.OpenDB(t)
	ctx := context.Background()
	svc := New(db, nil, passSealer{}).(*service)

	c := &Certificate{
		ID: "c-pem", PrimaryDomain: "pem.com", Domains: []string{"pem.com"},
		Source: SourceManual, Validation: ValidationManual, Status: StatusIssued,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	sealedCert, _ := svc.vault.SealSecret([]byte("CERT-PEM"))
	sealedKey, _ := svc.vault.SealSecret([]byte("KEY-PEM"))
	if err := svc.store.insertWithPEM(ctx, c, sealedCert, sealedKey); err != nil {
		t.Fatalf("insertWithPEM: %v", err)
	}
	gotCert, gotKey, err := svc.OpenCertPEM(ctx, "c-pem")
	if err != nil || gotCert != "CERT-PEM" || gotKey != "KEY-PEM" {
		t.Fatalf("OpenCertPEM 应返回明文对,得 %q/%q/%v", gotCert, gotKey, err)
	}
	if _, _, err := svc.OpenCertPEM(ctx, "nope"); err == nil || err != ErrNotFound {
		t.Fatalf("不存在证书应 ErrNotFound,得 %v", err)
	}
}
