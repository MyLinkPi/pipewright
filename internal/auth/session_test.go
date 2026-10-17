package auth

import (
	"errors"
	"testing"

	"github.com/huangchengsir/pipewright/internal/storetest"
)

// TestSessionTokenStoredAsHash sessions.token 列只存 sha256 hex(绝非原始 token),
// 防 DB 泄漏后库内值可直接当 cookie 冒充;对外接口仍以原始 token 为口径。
func TestSessionTokenStoredAsHash(t *testing.T) {
	st := storetest.Open(t)
	ss := NewSessionStore(st.DB)

	s1, err := ss.Create(RoleAdmin, "", "admin")
	if err != nil {
		t.Fatalf("Create(s1): %v", err)
	}
	s2, err := ss.Create(RoleMember, "u1", "alice")
	if err != nil {
		t.Fatalf("Create(s2): %v", err)
	}

	// 库内值 = hashToken(原始 token),且与原始 token 不同。
	var stored1 string
	if err := st.DB.QueryRow(`SELECT token FROM sessions WHERE csrf_token = ?`, s1.CSRFToken).Scan(&stored1); err != nil {
		t.Fatalf("read stored token: %v", err)
	}
	if stored1 == s1.Token {
		t.Fatal("sessions.token must not store the raw token")
	}
	if stored1 != hashToken(s1.Token) {
		t.Fatalf("stored token = %q, want hashToken(s1.Token)", stored1)
	}

	// Get 按原始 token 命中,并回填原始 token。
	got, err := ss.Get(s1.Token)
	if err != nil {
		t.Fatalf("Get(s1): %v", err)
	}
	if got.Token != s1.Token || got.Username != "admin" {
		t.Fatalf("Get(s1) = token=%q username=%q", got.Token, got.Username)
	}

	// List:两行,恰有一行 Current 且 id 与 SessionID(s1) 一致。
	metas, err := ss.List(s1.Token)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 2 {
		t.Fatalf("len(metas) = %d, want 2", len(metas))
	}
	current := 0
	for _, m := range metas {
		if m.Current {
			current++
			if m.ID != SessionID(s1.Token) {
				t.Fatalf("current id = %q, want SessionID(s1)", m.ID)
			}
		}
	}
	if current != 1 {
		t.Fatalf("current count = %d, want 1", current)
	}

	// DeleteByIDPrefix 按 SessionID 删除指定会话,另一会话不受影响。
	n, err := ss.DeleteByIDPrefix(SessionID(s2.Token))
	if err != nil {
		t.Fatalf("DeleteByIDPrefix(s2): %v", err)
	}
	if n != 1 {
		t.Fatalf("DeleteByIDPrefix(s2) rows = %d, want 1", n)
	}
	if _, err := ss.Get(s2.Token); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("s2 should be gone, got %v", err)
	}
	if _, err := ss.Get(s1.Token); err != nil {
		t.Fatalf("s1 should survive: %v", err)
	}

	// 非 hex 的 id(含 LIKE 通配符)不删任何行——防畸形前缀清空全表。
	for _, bad := range []string{"%", "_", "ZZ", "abc%", ""} {
		if n, err := ss.DeleteByIDPrefix(bad); err != nil || n != 0 {
			t.Fatalf("DeleteByIDPrefix(%q) = (%d, %v), want (0, nil)", bad, n, err)
		}
	}
	if _, err := ss.Get(s1.Token); err != nil {
		t.Fatalf("s1 must survive bad-prefix deletes: %v", err)
	}

	// Delete 仍按原始 token 删除。
	if err := ss.Delete(s1.Token); err != nil {
		t.Fatalf("Delete(s1): %v", err)
	}
	if _, err := ss.Get(s1.Token); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("s1 should be gone after Delete, got %v", err)
	}
}
