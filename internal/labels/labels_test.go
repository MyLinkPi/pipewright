package labels

import (
	"context"
	"errors"
	"testing"

	"github.com/huangchengsir/pipewright/internal/store"
	"github.com/huangchengsir/pipewright/internal/storetest"
	"github.com/huangchengsir/pipewright/internal/target"
)

// fakeLister 固定服务器清单(删除引用检查用)。
type fakeLister struct {
	servers []*target.Server
	err     error
}

func (f fakeLister) List(_ context.Context) ([]*target.Server, error) { return f.servers, f.err }

func testDB(t *testing.T) *store.Store {
	return storetest.Open(t)
}

func TestCreateListRoundTrip(t *testing.T) {
	st := testDB(t)
	svc := New(st.DB, nil)
	ctx := context.Background()

	for _, name := range []string{"linux", "arch=arm64", "gpu"} {
		if _, err := svc.Create(ctx, name); err != nil {
			t.Fatalf("create %q: %v", name, err)
		}
	}
	got, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 || got[0].Name != "arch=arm64" || got[1].Name != "gpu" || got[2].Name != "linux" {
		t.Fatalf("unexpected list: %+v", got)
	}
	if got[0].CreatedAt == "" {
		t.Fatal("created_at must be set")
	}
}

func TestCreateValidationAndDuplicate(t *testing.T) {
	st := testDB(t)
	svc := New(st.DB, nil)
	ctx := context.Background()

	for _, bad := range []string{"", "  ", "a b", "=v", "k=", "-lead", "大写中文"} {
		if _, err := svc.Create(ctx, bad); !errors.Is(err, ErrInvalidName) {
			t.Fatalf("create %q: want ErrInvalidName, got %v", bad, err)
		}
	}
	if _, err := svc.Create(ctx, "linux"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.Create(ctx, " linux "); err == nil || !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate(名字 trim 后同): want ErrExists, got %v", err)
	}
}

func TestDeleteBlockedWhileInUse(t *testing.T) {
	st := testDB(t)
	lister := fakeLister{servers: []*target.Server{
		{ID: "s1", Labels: "linux,arch=arm64"},
		{ID: "s2", Labels: "linux"},
		{ID: "s3", Labels: ""},
	}}
	svc := New(st.DB, lister)
	ctx := context.Background()

	if _, err := svc.Create(ctx, "linux"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.Create(ctx, "gpu"); err != nil {
		t.Fatalf("create悬置: %v", err)
	}

	err := svc.Delete(ctx, "linux")
	var inUse *InUseError
	if !errors.As(err, &inUse) || inUse.Servers != 2 {
		t.Fatalf("delete in-use: want InUseError{2}, got %v", err)
	}
	// 悬置标签可直接删;再删一次 → ErrNotFound。
	if err := svc.Delete(ctx, "gpu"); err != nil {
		t.Fatalf("delete悬置: %v", err)
	}
	if err := svc.Delete(ctx, "gpu"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing: want ErrNotFound, got %v", err)
	}
	// linux 仍在。
	got, _ := svc.List(ctx)
	if len(got) != 1 || got[0].Name != "linux" {
		t.Fatalf("unexpected list after deletes: %+v", got)
	}
}

func TestDeleteWithoutListerSkipsUsageCheck(t *testing.T) {
	st := testDB(t)
	svc := New(st.DB, nil)
	ctx := context.Background()
	if _, err := svc.Create(ctx, "linux"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.Delete(ctx, "linux"); err != nil {
		t.Fatalf("delete: %v", err)
	}
}
