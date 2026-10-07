package target

// jumps_test.go 是 SSH 跳板链(多跳登录)的领域层单测:
//   - 跳板链的持久化 roundtrip(归一/回读/Update 三态:nil 不动 / 非 nil 替换 / 空清空);
//   - validateJumps 校验(缺字段/端口越界/超跳数上限);
//   - 跳板凭据存在性与类型守卫(ssh_key / ssh_password 之外拒绝);
//   - Exec 装配:主凭据与各跳凭据独立解析,逐跳 Addr/User/明文形态正确(fake dialer 捕获);
//   - jumps 列 JSON 序列化边界(空/损坏一律视为直连)。
//
// 真实转发链路的传输级验证见 jumps_e2e_test.go(PIPEWRIGHT_E2E_SSH=1 门控)。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/vault"
)

const fakePEMMain = "-----BEGIN OPENSSH PRIVATE KEY-----\nmain-key-body\n-----END OPENSSH PRIVATE KEY-----"
const fakePEMHop2 = "-----BEGIN OPENSSH PRIVATE KEY-----\nhop2-key-body\n-----END OPENSSH PRIVATE KEY-----"

func TestJumpPersistenceRoundtrip(t *testing.T) {
	db := testDB(t)
	v := vault.New(db, testMasterKey())
	credID := newSSHCred(t, v, "main-pw")
	hopCred1 := newSSHCred(t, v, "hop1-pw")
	hopCred2 := newSSHCred(t, v, fakePEMHop2)
	svc := New(db, v, &capturingDialer{})

	srv, err := svc.Create(context.Background(), CreateInput{
		Name: "prod", Host: "10.0.0.9", User: "deploy", CredentialID: credID,
		Jumps: []ServerJump{
			// 归一:主机/用户去空白,端口 0 → DefaultPort。
			{Host: " bastion.corp ", Port: 0, User: " ops ", CredentialID: hopCred1},
			{Host: "10.0.1.1", Port: 2222, User: "jump", CredentialID: hopCred2},
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(srv.Jumps) != 2 {
		t.Fatalf("jumps len = %d, want 2", len(srv.Jumps))
	}
	if srv.Jumps[0].Host != "bastion.corp" || srv.Jumps[0].Port != DefaultPort || srv.Jumps[0].User != "ops" {
		t.Fatalf("jump0 归一错误: %+v", srv.Jumps[0])
	}
	if srv.Jumps[1].CredentialID != hopCred2 {
		t.Fatalf("jump1 credentialId = %q, want %q", srv.Jumps[1].CredentialID, hopCred2)
	}

	got, err := svc.Get(context.Background(), srv.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Jumps) != 2 || got.Jumps[1].Host != "10.0.1.1" {
		t.Fatalf("Get 回读跳板链不一致: %+v", got.Jumps)
	}

	list, err := svc.List(context.Background())
	if err != nil || len(list) != 1 {
		t.Fatalf("List: %v / len=%d", err, len(list))
	}
	if len(list[0].Jumps) != 2 {
		t.Fatalf("List 回读跳板链不一致: %+v", list[0].Jumps)
	}

	// Update nil = 不修改。
	newName := "prod-renamed"
	upd, err := svc.Update(context.Background(), srv.ID, UpdateInput{Name: &newName})
	if err != nil {
		t.Fatalf("Update(name): %v", err)
	}
	if len(upd.Jumps) != 2 {
		t.Fatalf("nil jumps 被误改: %+v", upd.Jumps)
	}

	// Update 非 nil = 整体替换。
	one := []ServerJump{{Host: "single", Port: 2200, User: "u", CredentialID: hopCred1}}
	upd2, err := svc.Update(context.Background(), srv.ID, UpdateInput{Jumps: &one})
	if err != nil {
		t.Fatalf("Update(jumps): %v", err)
	}
	if len(upd2.Jumps) != 1 || upd2.Jumps[0].Host != "single" || upd2.Jumps[0].Port != 2200 {
		t.Fatalf("整体替换失败: %+v", upd2.Jumps)
	}

	// Update 空切片 = 清空为直连。
	empty := []ServerJump{}
	upd3, err := svc.Update(context.Background(), srv.ID, UpdateInput{Jumps: &empty})
	if err != nil {
		t.Fatalf("Update(empty jumps): %v", err)
	}
	if len(upd3.Jumps) != 0 {
		t.Fatalf("空切片未清空: %+v", upd3.Jumps)
	}
}

func TestJumpValidation(t *testing.T) {
	db := testDB(t)
	v := vault.New(db, testMasterKey())
	credID := newSSHCred(t, v, "main-pw")
	hopCred := newSSHCred(t, v, "hop-pw")
	svc := New(db, v, &capturingDialer{})

	cases := []struct {
		name  string
		jumps []ServerJump
	}{
		{"缺主机", []ServerJump{{Host: "", Port: 22, User: "u", CredentialID: hopCred}}},
		{"缺用户", []ServerJump{{Host: "h", Port: 22, User: "", CredentialID: hopCred}}},
		{"缺凭据", []ServerJump{{Host: "h", Port: 22, User: "u", CredentialID: ""}}},
		{"端口越界", []ServerJump{{Host: "h", Port: 70000, User: "u", CredentialID: hopCred}}},
		{"超过上限", func() []ServerJump {
			jumps := make([]ServerJump, MaxJumps+1)
			for i := range jumps {
				jumps[i] = ServerJump{Host: "h", Port: 22, User: "u", CredentialID: hopCred}
			}
			return jumps
		}()},
	}
	for _, tc := range cases {
		t.Run("create/"+tc.name, func(t *testing.T) {
			_, err := svc.Create(context.Background(), CreateInput{
				Name: "x", Host: "10.0.0.1", User: "deploy", CredentialID: credID, Jumps: tc.jumps,
			})
			if !errors.Is(err, ErrInvalidJump) {
				t.Fatalf("err = %v, want ErrInvalidJump", err)
			}
		})
	}

	srv, err := svc.Create(context.Background(), CreateInput{
		Name: "ok", Host: "10.0.0.1", User: "deploy", CredentialID: credID,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, tc := range cases {
		t.Run("update/"+tc.name, func(t *testing.T) {
			in := tc.jumps
			_, err := svc.Update(context.Background(), srv.ID, UpdateInput{Jumps: &in})
			if !errors.Is(err, ErrInvalidJump) {
				t.Fatalf("err = %v, want ErrInvalidJump", err)
			}
		})
	}
}

func TestJumpCredentialGuards(t *testing.T) {
	db := testDB(t)
	v := vault.New(db, testMasterKey())
	credID := newSSHCred(t, v, "main-pw")
	svc := New(db, v, &capturingDialer{})

	t.Run("凭据不存在", func(t *testing.T) {
		jumps := []ServerJump{{Host: "h", Port: 22, User: "u", CredentialID: "no-such-cred"}}
		_, err := svc.Create(context.Background(), CreateInput{
			Name: "x", Host: "10.0.0.1", User: "deploy", CredentialID: credID, Jumps: jumps,
		})
		if !errors.Is(err, ErrCredentialNotFound) {
			t.Fatalf("err = %v, want ErrCredentialNotFound", err)
		}
	})

	t.Run("类型错配(git token 不可作跳板凭据)", func(t *testing.T) {
		gitCred, err := v.Create(vault.CreateInput{Name: "gt", Type: vault.TypeGitToken, Secret: "tok"})
		if err != nil {
			t.Fatalf("vault.Create: %v", err)
		}
		jumps := []ServerJump{{Host: "h", Port: 22, User: "u", CredentialID: gitCred.ID}}
		_, err = svc.Create(context.Background(), CreateInput{
			Name: "x", Host: "10.0.0.1", User: "deploy", CredentialID: credID, Jumps: jumps,
		})
		if !errors.Is(err, ErrCredentialTypeMismatch) {
			t.Fatalf("err = %v, want ErrCredentialTypeMismatch", err)
		}
	})
}

// TestExecAssemblesJumpChain 验证 Exec 装配:主凭据与各跳凭据独立解析,逐跳
// Addr/User/认证形态(PEM → PrivateKey,口令 → Password)正确,目标 addr 不受影响。
func TestExecAssemblesJumpChain(t *testing.T) {
	db := testDB(t)
	v := vault.New(db, testMasterKey())
	mainCred := newSSHCred(t, v, fakePEMMain)
	hop1Cred := newSSHCred(t, v, "hop1-password")
	hop2Cred := newSSHCred(t, v, fakePEMHop2)
	d := &capturingDialer{res: &ExecResult{Stdout: "ok"}}
	svc := New(db, v, d)

	srv, err := svc.Create(context.Background(), CreateInput{
		Name: "prod", Host: "10.0.0.9", Port: 2200, User: "deploy", CredentialID: mainCred,
		Jumps: []ServerJump{
			{Host: "bastion.corp", Port: 22, User: "ops", CredentialID: hop1Cred},
			{Host: "10.0.1.1", Port: 2222, User: "jump", CredentialID: hop2Cred},
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := svc.Exec(context.Background(), srv.ID, []string{"echo", "ok"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if d.gotAddr != "10.0.0.9:2200" {
		t.Fatalf("目标 addr = %q, want 10.0.0.9:2200", d.gotAddr)
	}
	if len(d.gotCfg.Jumps) != 2 {
		t.Fatalf("jumps len = %d, want 2", len(d.gotCfg.Jumps))
	}
	hop0 := d.gotCfg.Jumps[0]
	if hop0.Addr != "bastion.corp:22" || hop0.User != "ops" {
		t.Fatalf("hop0 = %+v", hop0)
	}
	if hop0.Password != "hop1-password" || hop0.PrivateKey != "" {
		t.Fatalf("hop0 认证形态错误(应为口令): %+v", hop0)
	}
	hop1 := d.gotCfg.Jumps[1]
	if hop1.Addr != "10.0.1.1:2222" || hop1.User != "jump" {
		t.Fatalf("hop1 = %+v", hop1)
	}
	if !strings.Contains(hop1.PrivateKey, "hop2-key-body") || hop1.Password != "" {
		t.Fatalf("hop1 认证形态错误(应为 PEM 私钥): %+v", hop1)
	}
	if !strings.Contains(d.gotCfg.PrivateKey, "main-key-body") || d.gotCfg.Password != "" {
		t.Fatalf("主凭据认证形态错误(应为 PEM 私钥): %+v", d.gotCfg)
	}
}

func TestJumpsJSONRoundtrip(t *testing.T) {
	if got := marshalJumps(nil); got != "[]" {
		t.Fatalf("marshalJumps(nil) = %q, want []", got)
	}
	jumps := []ServerJump{{Host: "h", Port: 22, User: "u", CredentialID: "c"}}
	got := parseJumps(marshalJumps(jumps))
	if len(got) != 1 || got[0] != jumps[0] {
		t.Fatalf("roundtrip 不一致: %+v", got)
	}
	// 空/损坏一律视为直连。
	for _, raw := range []string{"", "null", "[]", "not-json", `{"host":"h"}`} {
		if p := parseJumps(raw); p != nil {
			t.Fatalf("parseJumps(%q) = %+v, want nil", raw, p)
		}
	}
}
