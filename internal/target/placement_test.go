package target

import (
	"context"
	"testing"

	"github.com/huangchengsir/pipewright/internal/vault"
)

// TestPlacementRoundTrip 验证放置字段全链路:默认登记 → default 池 + standard;
// SetPlacement 归入 pve 池后 Get/List 可读,且 pve_node 化时构建机池字段被清空;
// 回退 standard/default 保留其他字段。
func TestPlacementRoundTrip(t *testing.T) {
	db := testDB(t)
	v := vault.New(db, testMasterKey())
	credID := newSSHCred(t, v, "secret-pw")
	svc := New(db, v, &capturingDialer{})
	ctx := context.Background()

	// 造一个 pve 池(模拟 PVE 探测流建的池;直接 INSERT,池 CRUD 归探测流)。
	poolID := "pool-pve-test"
	_, err := db.Exec(`INSERT INTO server_pools (id, name, kind, cluster_id, created_at, updated_at)
		VALUES (?, 'pve-lab', 'pve', 'cl-1', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`, poolID)
	if err != nil {
		t.Fatalf("seed pool: %v", err)
	}

	// 普通登记:默认池 + standard。
	srv, err := svc.Create(ctx, CreateInput{
		Name: "pve-node-1", Host: "10.0.0.21", User: "root", CredentialID: credID,
		Labels: "linux,arch=amd64", MaxBuilds: 4, Priority: 10,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if srv.PoolID != DefaultPoolID || srv.Kind != KindStandard || srv.PoolName != "default" {
		t.Fatalf("default placement mismatch: pool=%q kind=%q poolName=%q", srv.PoolID, srv.Kind, srv.PoolName)
	}

	// 归入 pve 池:kind=pve_node 且构建机池字段被清空。
	if err := svc.SetPlacement(ctx, srv.ID, poolID, KindPVENode); err != nil {
		t.Fatalf("SetPlacement: %v", err)
	}
	got, err := svc.Get(ctx, srv.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.PoolID != poolID || got.Kind != KindPVENode || !got.IsPVENode() {
		t.Fatalf("pve placement mismatch: %+v", got)
	}
	if got.PoolName != "pve-lab" {
		t.Fatalf("poolName = %q, want pve-lab", got.PoolName)
	}
	if got.Labels != "" || got.MaxBuilds != 0 {
		t.Fatalf("pve_node must clear runner pool fields: labels=%q maxBuilds=%d", got.Labels, got.MaxBuilds)
	}
	list, err := svc.List(ctx)
	if err != nil || len(list) != 1 || !list[0].IsPVENode() {
		t.Fatalf("List: %v / %+v", err, list)
	}

	// 回退 standard/default:池/类别还原,其余字段不动。
	if err := svc.SetPlacement(ctx, srv.ID, DefaultPoolID, KindStandard); err != nil {
		t.Fatalf("SetPlacement revert: %v", err)
	}
	got, err = svc.Get(ctx, srv.ID)
	if err != nil {
		t.Fatalf("Get after revert: %v", err)
	}
	if got.PoolID != DefaultPoolID || got.Kind != KindStandard || got.IsPVENode() {
		t.Fatalf("revert mismatch: %+v", got)
	}
}

// TestCreateWithPlacement 验证 Create 直接带放置字段(探测流建兄弟节点占位的路径)
// 与非法 kind/不存在池的拒绝。
func TestCreateWithPlacement(t *testing.T) {
	db := testDB(t)
	v := vault.New(db, testMasterKey())
	credID := newSSHCred(t, v, "secret-pw")
	svc := New(db, v, &capturingDialer{})
	ctx := context.Background()

	poolID := "pool-pve-2"
	if _, err := db.Exec(`INSERT INTO server_pools (id, name, kind, cluster_id, created_at, updated_at)
		VALUES (?, 'pve-lab2', 'pve', 'cl-2', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`, poolID); err != nil {
		t.Fatalf("seed pool: %v", err)
	}

	srv, err := svc.Create(ctx, CreateInput{
		Name: "pve-node-2", Host: "10.0.0.22", User: "root", CredentialID: credID,
		PoolID: poolID, Kind: KindPVENode,
	})
	if err != nil {
		t.Fatalf("Create with placement: %v", err)
	}
	if !srv.IsPVENode() || srv.PoolID != poolID {
		t.Fatalf("placement mismatch: %+v", srv)
	}

	// 非法 kind。
	if _, err := svc.Create(ctx, CreateInput{
		Name: "bad", Host: "10.0.0.23", User: "root", CredentialID: credID, Kind: "hypervisor",
	}); err != ErrInvalidKind {
		t.Fatalf("invalid kind err = %v, want ErrInvalidKind", err)
	}
	// 不存在的池。
	if _, err := svc.Create(ctx, CreateInput{
		Name: "bad2", Host: "10.0.0.24", User: "root", CredentialID: credID, PoolID: "no-such-pool",
	}); err != ErrPoolNotFound {
		t.Fatalf("unknown pool err = %v, want ErrPoolNotFound", err)
	}
	// SetPlacement 对不存在服务器。
	if err := svc.SetPlacement(ctx, "no-such-server", poolID, KindPVENode); err != ErrNotFound {
		t.Fatalf("SetPlacement missing server err = %v, want ErrNotFound", err)
	}
	// SetPlacement 非法池。
	if err := svc.SetPlacement(ctx, srv.ID, "no-such-pool", KindPVENode); err != ErrPoolNotFound {
		t.Fatalf("SetPlacement unknown pool err = %v, want ErrPoolNotFound", err)
	}
}

// TestListPools 验证池列表(default 单例 + 计数 + 创建序)。
func TestListPools(t *testing.T) {
	db := testDB(t)
	v := vault.New(db, testMasterKey())
	credID := newSSHCred(t, v, "secret-pw")
	svc := New(db, v, &capturingDialer{})
	ctx := context.Background()

	pools, err := svc.ListPools(ctx)
	if err != nil || len(pools) != 1 {
		t.Fatalf("ListPools default: %v / %+v", err, pools)
	}
	if pools[0].ID != DefaultPoolID || pools[0].Kind != KindStandard {
		t.Fatalf("default pool mismatch: %+v", pools[0])
	}

	// 造 pve 池 + 两台 pve 机器 + 一台普通机器,验证计数。
	if _, err := db.Exec(`INSERT INTO server_pools (id, name, kind, cluster_id, created_at, updated_at)
		VALUES ('p3', 'pve-lab3', 'pve', 'cl-3', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed pool: %v", err)
	}
	for i, host := range []string{"10.9.0.1", "10.9.0.2"} {
		if _, err := svc.Create(ctx, CreateInput{
			Name: "pve-n", Host: host, User: "root", CredentialID: credID, PoolID: "p3", Kind: KindPVENode,
		}); err != nil {
			t.Fatalf("Create pve node %d: %v", i, err)
		}
	}
	if _, err := svc.Create(ctx, CreateInput{
		Name: "std", Host: "10.9.0.3", User: "root", CredentialID: credID,
	}); err != nil {
		t.Fatalf("Create std: %v", err)
	}

	pools, err = svc.ListPools(ctx)
	if err != nil {
		t.Fatalf("ListPools: %v", err)
	}
	if len(pools) != 2 {
		t.Fatalf("pools = %d, want 2", len(pools))
	}
	byID := map[string]int{}
	for _, p := range pools {
		byID[p.ID] = p.ServerCount
	}
	if byID[DefaultPoolID] != 1 || byID["p3"] != 2 {
		t.Fatalf("counts = %v, want default=1 p3=2", byID)
	}
}
