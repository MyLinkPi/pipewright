package meshdns

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/dnsprovider"
	"github.com/huangchengsir/pipewright/internal/storetest"
)

// --- 桩 ProviderSource(不触真网络)------------------------------------------

// stubSource 记录 EnsureSubdomainRecord 调用,可配置逐域名错误;Get 返回固定提供商。
type stubSource struct {
	provider *dnsprovider.Provider
	gone     bool // true → Get 返回 ErrNotFound
	ensure   []ensureCall
	errBy    map[string]error // fqdn → 错误(缺省 nil)
}

type ensureCall struct{ providerID, fqdn, ip string }

func (s *stubSource) Get(_ context.Context, _ string) (*dnsprovider.Provider, error) {
	if s.gone {
		return nil, dnsprovider.ErrNotFound
	}
	return s.provider, nil
}

func (s *stubSource) EnsureSubdomainRecord(_ context.Context, providerID, fqdn, ip string) error {
	s.ensure = append(s.ensure, ensureCall{providerID, fqdn, ip})
	if s.errBy != nil {
		return s.errBy[fqdn]
	}
	return nil
}

func newFixture(t *testing.T, baseDomains ...string) (Service, *stubSource) {
	t.Helper()
	p := &dnsprovider.Provider{ID: "prov-1", Type: "cloudflare", Name: "CF"}
	for _, d := range baseDomains {
		p.Zones = append(p.Zones, dnsprovider.Zone{ProviderID: p.ID, BaseDomain: d})
	}
	src := &stubSource{provider: p}
	db := storetest.OpenDB(t)
	return New(db, src), src
}

// --- Service CRUD ------------------------------------------------------------

func TestCreateValidation(t *testing.T) {
	ctx := context.Background()
	svc, _ := newFixture(t, "example.com", "preview.example.com")

	// 正常:归一化小写,回显根区无关字段。
	r, err := svc.Create(ctx, CreateInput{ProviderID: "prov-1", FQDN: "Home.Example.com", Enabled: true})
	if err != nil || r.FQDN != "home.example.com" || !r.Enabled {
		t.Fatalf("Create: %v / %+v", err, r)
	}
	// 不在根区下 / 等于根区 → ErrZoneNotFound。
	if _, err := svc.Create(ctx, CreateInput{ProviderID: "prov-1", FQDN: "x.example.org"}); !errors.Is(err, ErrZoneNotFound) {
		t.Fatalf("不覆盖应 ErrZoneNotFound, got %v", err)
	}
	if _, err := svc.Create(ctx, CreateInput{ProviderID: "prov-1", FQDN: "example.com"}); !errors.Is(err, ErrZoneNotFound) {
		t.Fatalf("等于根区应 ErrZoneNotFound, got %v", err)
	}
	// 非法格式 / 缺 providerId → ErrInvalidInput。
	if _, err := svc.Create(ctx, CreateInput{ProviderID: "prov-1", FQDN: "http://x.example.com"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("非法格式应 ErrInvalidInput, got %v", err)
	}
	if _, err := svc.Create(ctx, CreateInput{FQDN: "a.example.com"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("缺 providerId 应 ErrInvalidInput, got %v", err)
	}
	// fqdn 重复 → ErrDuplicate。
	if _, err := svc.Create(ctx, CreateInput{ProviderID: "prov-1", FQDN: "home.example.com"}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("重复应 ErrDuplicate, got %v", err)
	}
}

func TestCreateProviderGone(t *testing.T) {
	ctx := context.Background()
	svc, src := newFixture(t, "example.com")
	src.gone = true
	if _, err := svc.Create(ctx, CreateInput{ProviderID: "prov-1", FQDN: "a.example.com"}); !errors.Is(err, ErrProviderNotFound) {
		t.Fatalf("提供商已删应 ErrProviderNotFound, got %v", err)
	}
}

func TestUpdateAndListEcho(t *testing.T) {
	ctx := context.Background()
	svc, _ := newFixture(t, "example.com")
	r, _ := svc.Create(ctx, CreateInput{ProviderID: "prov-1", FQDN: "a.example.com", Enabled: true})

	// 改域名(须过覆盖校验)+ 停用。
	newFQDN := "b.example.com"
	off := false
	got, err := svc.Update(ctx, r.ID, UpdateInput{FQDN: &newFQDN, Enabled: &off})
	if err != nil || got.FQDN != "b.example.com" || got.Enabled {
		t.Fatalf("Update: %v / %+v", err, got)
	}
	bad := "x.example.org"
	if _, err := svc.Update(ctx, r.ID, UpdateInput{FQDN: &bad}); !errors.Is(err, ErrZoneNotFound) {
		t.Fatalf("改到不覆盖域名应 ErrZoneNotFound, got %v", err)
	}
	// 域名换后被占用 → ErrDuplicate。
	if _, err := svc.Create(ctx, CreateInput{ProviderID: "prov-1", FQDN: "b.example.com"}); err == nil {
		t.Fatalf("占用域名应报错")
	}

	// List 回显提供商名称/类型;提供商删除后 → ProviderGone。
	items, err := svc.List(ctx)
	if err != nil || len(items) != 1 || items[0].ProviderName != "CF" || items[0].ProviderType != "cloudflare" || items[0].ProviderGone {
		t.Fatalf("List 回显: %v / %+v", err, items)
	}
	if err := svc.Delete(ctx, r.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := svc.Delete(ctx, r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重复删应 ErrNotFound, got %v", err)
	}
}

// --- Syncer ------------------------------------------------------------------

func newSyncerFor(t *testing.T, svc Service, src *stubSource, ip string) *Syncer {
	t.Helper()
	return NewSyncer(svc, src,
		func(context.Context) string { return ip },
		func(context.Context) bool { return true })
}

func TestSyncerFollowsIPChange(t *testing.T) {
	ctx := context.Background()
	svc, src := newFixture(t, "example.com")
	r1, _ := svc.Create(ctx, CreateInput{ProviderID: "prov-1", FQDN: "a.example.com", Enabled: true})
	r2, _ := svc.Create(ctx, CreateInput{ProviderID: "prov-1", FQDN: "b.example.com", Enabled: true})
	off, _ := svc.Create(ctx, CreateInput{ProviderID: "prov-1", FQDN: "c.example.com"})
	_ = off

	// 首轮:启用条目全部同步,停用条目不动。
	sy := newSyncerFor(t, svc, src, "10.144.0.5")
	sy.syncAuto(ctx)
	if len(src.ensure) != 2 {
		t.Fatalf("应同步 2 条启用条目, got %v", src.ensure)
	}
	items, _ := svc.List(ctx)
	byID := map[string]Record{}
	for _, it := range items {
		byID[it.ID] = it
	}
	if byID[r1.ID].LastIP != "10.144.0.5" || byID[r1.ID].LastError != "" || byID[r1.ID].LastSyncedAt == "" {
		t.Fatalf("成功同步应回写状态: %+v", byID[r1.ID])
	}

	// IP 未变:节拍内不重复触达 API(兜底周期 12h 未到)。
	src.ensure = nil
	sy.syncAuto(ctx)
	if len(src.ensure) != 0 {
		t.Fatalf("IP 未变不应重复同步, got %v", src.ensure)
	}

	// IP 变化:全部启用条目跟随更新。
	sy2 := newSyncerFor(t, svc, src, "10.144.0.9")
	sy2.syncAuto(ctx)
	if len(src.ensure) != 2 {
		t.Fatalf("IP 变化应重同步, got %v", src.ensure)
	}
	for _, c := range src.ensure {
		if c.ip != "10.144.0.9" {
			t.Fatalf("应写入新 IP: %+v", c)
		}
	}
	items, _ = svc.List(ctx)
	for _, it := range items {
		if it.ID == r2.ID && it.LastIP != "10.144.0.9" {
			t.Fatalf("IP 变化应回写: %+v", it)
		}
	}
}

func TestSyncerFailureRecordsErrorAndBackoff(t *testing.T) {
	ctx := context.Background()
	svc, src := newFixture(t, "example.com")
	src.errBy = map[string]error{"a.example.com": errors.New("boom")}
	_, _ = svc.Create(ctx, CreateInput{ProviderID: "prov-1", FQDN: "a.example.com", Enabled: true})

	sy := newSyncerFor(t, svc, src, "10.144.0.5")
	sy.syncAuto(ctx)
	items, _ := svc.List(ctx)
	if items[0].LastError == "" || items[0].LastSyncedAt != "" {
		t.Fatalf("失败应记 last_error 且不置 last_synced_at: %+v", items[0])
	}

	// 失败退避:同 IP 下轮自动节拍不重试(手动 SyncNow 不受限,仍重试)。
	src.ensure = nil
	sy.syncAuto(ctx)
	if len(src.ensure) != 0 {
		t.Fatalf("失败条目应受退避限制, got %v", src.ensure)
	}
	ip, results := sy.SyncNow(ctx)
	if ip != "10.144.0.5" || len(results) != 1 || results[0].OK || results[0].Error == "" {
		t.Fatalf("SyncNow 应逐条返回失败结果: %v / %+v", ip, results)
	}
	if len(src.ensure) != 1 {
		t.Fatalf("手动同步不受退避限制, got %v", src.ensure)
	}
}

func TestSyncerNoIPIsNoop(t *testing.T) {
	ctx := context.Background()
	svc, src := newFixture(t, "example.com")
	_, _ = svc.Create(ctx, CreateInput{ProviderID: "prov-1", FQDN: "a.example.com", Enabled: true})

	sy := NewSyncer(svc, src, func(context.Context) string { return "" }, func(context.Context) bool { return true })
	sy.syncAuto(ctx)
	ip, results := sy.SyncNow(ctx)
	if ip != "" || results != nil {
		t.Fatalf("无虚拟 IP 应空转: %v / %+v", ip, results)
	}
	if len(src.ensure) != 0 {
		t.Fatalf("无 IP 不应触达 API, got %v", src.ensure)
	}
}

func TestSyncerReEnsureAfterInterval(t *testing.T) {
	ctx := context.Background()
	svc, src := newFixture(t, "example.com")
	r, _ := svc.Create(ctx, CreateInput{ProviderID: "prov-1", FQDN: "a.example.com", Enabled: true})

	sy := newSyncerFor(t, svc, src, "10.144.0.5")
	sy.syncAuto(ctx)
	if len(src.ensure) != 1 {
		t.Fatalf("首轮应同步, got %v", src.ensure)
	}
	// 直接把 last_synced_at 拨回 13h 前(store 层写库模拟时间流逝),同 IP 也应兜底重同步。
	old := time.Now().UTC().Add(-13 * time.Hour).Format(time.RFC3339)
	if _, err := svc.(*service).store.DB.ExecContext(ctx,
		`UPDATE mesh_dns_records SET last_synced_at = ? WHERE id = ?`, old, r.ID); err != nil {
		t.Fatalf("回拨时间: %v", err)
	}
	src.ensure = nil
	sy.syncAuto(ctx)
	if len(src.ensure) != 1 {
		t.Fatalf("超过兜底周期应重同步, got %v", src.ensure)
	}
}
