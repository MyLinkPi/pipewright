package certmgmt

import (
	"context"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/store"
	"github.com/huangchengsir/pipewright/internal/storetest"
)

// TestCertStoreRoundtrip 验证证书行 落库 → 列 → 尝试 → 成功回写 → 密文读 → 详情追加 →
// 自动续期开关 → 删 在真库上往返一致(两方言)。
func TestCertStoreRoundtrip(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		ctx := context.Background()
		s := NewStore(st.DB)

		c := &Certificate{
			ID: "cert-1", PrimaryDomain: "*.efg.com", Domains: []string{"*.efg.com", "efg.com"},
			Source: SourceACME, CA: CALetsEncrypt, Validation: ValidationDNS, DNSProviderID: "p1",
			KeyType: KeyTypeEC256, AutoRenew: true, Status: StatusPending, StatusDetail: "已排队",
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if err := s.insert(ctx, c); err != nil {
			t.Fatalf("insert: %v", err)
		}

		got, err := s.get(ctx, c.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.PrimaryDomain != "*.efg.com" || len(got.Domains) != 2 || !got.AutoRenew || got.Status != StatusPending || got.HasPEM {
			t.Fatalf("落库字段不符: %+v", got)
		}

		// 主域唯一冲突。
		if err := s.insert(ctx, &Certificate{ID: "cert-2", PrimaryDomain: "*.efg.com",
			Source: SourceACME, Validation: ValidationDNS, Status: StatusPending,
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != ErrDomainTaken {
			t.Fatalf("主域冲突应 ErrDomainTaken, got %v", err)
		}

		// 尝试中。
		if err := s.markAttempt(ctx, c.ID, "签发中…"); err != nil {
			t.Fatalf("markAttempt: %v", err)
		}
		got, _ = s.get(ctx, c.ID)
		if got.Status != StatusPending || got.StatusDetail != "签发中…" || got.LastAttemptAt.IsZero() {
			t.Fatalf("markAttempt 后状态不符: %+v", got)
		}

		// 成功回写(密文 + 元数据)。
		meta := certMeta{subject: "CN=*.efg.com", issuer: "Let's Encrypt", notBefore: time.Now().UTC(), notAfter: time.Now().Add(90 * 24 * time.Hour).UTC()}
		if err := s.markIssued(ctx, c.ID, []byte("sealed-cert"), []byte("sealed-key"), meta, "签发成功"); err != nil {
			t.Fatalf("markIssued: %v", err)
		}
		got, _ = s.get(ctx, c.ID)
		if got.Status != StatusIssued || !got.HasPEM || got.Issuer != "Let's Encrypt" || got.NotAfter.IsZero() || got.LastIssuedAt.IsZero() {
			t.Fatalf("markIssued 后状态不符: %+v", got)
		}

		// 密文读回。
		sc, sk, ok, err := s.getSealed(ctx, c.ID)
		if err != nil || !ok || string(sc) != "sealed-cert" || string(sk) != "sealed-key" {
			t.Fatalf("getSealed 不符: %v %q %q", err, sc, sk)
		}

		// 详情追加(截断安全)。
		if err := s.appendStatusDetail(ctx, c.ID, "下发提示:部分失败"); err != nil {
			t.Fatalf("appendStatusDetail: %v", err)
		}
		got, _ = s.get(ctx, c.ID)
		if got.StatusDetail != "签发成功 下发提示:部分失败" {
			t.Fatalf("详情追加不符: %q", got.StatusDetail)
		}

		// 失败回写 → 状态 failed(密文仍在)。
		if err := s.markFailed(ctx, c.ID, "boom"); err != nil {
			t.Fatalf("markFailed: %v", err)
		}
		got, _ = s.get(ctx, c.ID)
		if got.Status != StatusFailed || got.StatusDetail != "boom" || !got.HasPEM {
			t.Fatalf("markFailed 后状态不符: %+v", got)
		}

		// 自动续期开关 + 列表。
		if err := s.setAutoRenew(ctx, c.ID, false); err != nil {
			t.Fatalf("setAutoRenew: %v", err)
		}
		list, err := s.list(ctx)
		if err != nil || len(list) != 1 || list[0].AutoRenew {
			t.Fatalf("list/setAutoRenew 不符: %v %+v", err, list)
		}

		// 删除。
		if err := s.delete(ctx, c.ID); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := s.get(ctx, c.ID); err != ErrNotFound {
			t.Fatalf("删除后应 ErrNotFound, got %v", err)
		}
		if err := s.delete(ctx, c.ID); err != ErrNotFound {
			t.Fatalf("重复删除应 ErrNotFound, got %v", err)
		}
	})
}

// TestBackfillFromServiceReg 验证 servicereg 既有手动证书幂等回填(source=manual,密文搬移,
// 按 primary_domain 去重)。
func TestBackfillFromServiceReg(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		ctx := context.Background()
		now := time.Now().UTC().Format(time.RFC3339)
		// 两行带证书的基域 + 一行无证书的基域。
		for _, base := range []string{"efg.com", "other.io"} {
			if _, err := st.DB.ExecContext(ctx,
				`INSERT INTO service_reg_domains (id, base_domain, cert_pem_sealed, key_pem_sealed,
				 cert_subject, cert_expires_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				"sr-"+base, base, []byte("sealed-c-"+base), []byte("sealed-k-"+base), "CN=*."+base,
				time.Now().Add(30*24*time.Hour).UTC().Format(time.RFC3339), now, now,
			); err != nil {
				t.Fatalf("seed %s: %v", base, err)
			}
		}
		if _, err := st.DB.ExecContext(ctx,
			`INSERT INTO service_reg_domains (id, base_domain, created_at, updated_at) VALUES ('sr-bare', 'bare.io', ?, ?)`,
			now, now); err != nil {
			t.Fatalf("seed bare: %v", err)
		}

		s := NewStore(st.DB)
		n, err := s.BackfillFromServiceReg(ctx)
		if err != nil {
			t.Fatalf("backfill: %v", err)
		}
		if n != 2 {
			t.Fatalf("应回填 2 张, got %d", n)
		}
		list, err := s.list(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(list) != 2 {
			t.Fatalf("回填后应有 2 行, got %d", len(list))
		}
		for _, c := range list {
			if c.Source != SourceManual || c.Validation != ValidationManual || c.AutoRenew || c.Status != StatusIssued {
				t.Fatalf("回填行字段不符: %+v", c)
			}
			sc, _, ok, _ := s.getSealed(ctx, c.ID)
			if !ok || string(sc) != "sealed-c-"+c.PrimaryDomain {
				t.Fatalf("回填密文不符: %s", c.PrimaryDomain)
			}
		}

		// 幂等:再来一遍不重复。
		if n, _ := s.BackfillFromServiceReg(ctx); n != 0 {
			t.Fatalf("二次回填应为 0, got %d", n)
		}
		if list, _ := s.list(ctx); len(list) != 2 {
			t.Fatalf("二次回填后仍应 2 行, got %d", len(list))
		}
	})
}
