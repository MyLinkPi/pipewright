package aliyunecs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/storetest"
	"github.com/huangchengsir/pipewright/internal/target"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// fakeEcs 是注入用的假 API 客户端:返回可变实例清单,可配置失败。
type fakeEcs struct {
	instances []Instance
	listErr   error
	verifyErr error
	// 开关机记录与可配置行为。
	started   []string
	stopped   []string
	stopForce []bool
	powerErr  map[string]error // instanceID → 发起动作即失败
	statuses  map[string]string
	statusErr error
	pollHook  func() // 每次状态轮询前触发(测试驱动状态迁移)
}

func (f *fakeEcs) ListInstances(context.Context) ([]Instance, error) { return f.instances, f.listErr }
func (f *fakeEcs) Verify(context.Context) error                      { return f.verifyErr }

func (f *fakeEcs) StartInstance(_ context.Context, id string) error {
	if err := f.powerErr[id]; err != nil {
		return err
	}
	f.started = append(f.started, id)
	return nil
}

func (f *fakeEcs) StopInstance(_ context.Context, id string, forceStop bool) error {
	if err := f.powerErr[id]; err != nil {
		return err
	}
	f.stopped = append(f.stopped, id)
	f.stopForce = append(f.stopForce, forceStop)
	return nil
}

func (f *fakeEcs) InstanceStatuses(_ context.Context, ids []string) (map[string]string, error) {
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	if f.pollHook != nil {
		f.pollHook()
	}
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		if st, ok := f.statuses[id]; ok {
			out[id] = st
		}
	}
	return out, nil
}

// testEnv 聚合一个完整测试环境:迁移库 + 真 vault + 真 target + 假 ECS 客户端。
type testEnv struct {
	db      *sql.DB
	vault   vault.Vault
	target  target.Service
	svc     Service
	fake    *fakeEcs
	account *Account
}

func testMasterKey() *[32]byte {
	var k [32]byte
	for i := range k {
		k[i] = byte(i + 17)
	}
	return &k
}

// newEnv 建环境并登记一个默认账户(cn-hangzhou)+ 一条 ssh_password 凭据。
func newEnv(t *testing.T) *testEnv {
	t.Helper()
	db := storetest.OpenDB(t)
	v := vault.New(db, testMasterKey())
	tgt := target.New(db, v, nil)
	fake := &fakeEcs{}
	s := New(db, v, tgt).(*service)
	s.newClient = func(ak, sk, region string) ecsAPI { return fake }
	acc, err := s.CreateAccount(context.Background(), AccountCreateInput{
		Name: "主账户", AccessKeyID: "LTAI5tFakeFake", Secret: "fake-secret-000", Region: "cn-hangzhou",
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	return &testEnv{db: db, vault: v, target: tgt, svc: s, fake: fake, account: acc}
}

// newSSHCred 经真 vault 建一条 ssh_password 凭据,返回 id。
func (e *testEnv) newSSHCred(t *testing.T, name string) string {
	t.Helper()
	cred, err := e.vault.Create(vault.CreateInput{Name: name, Type: vault.TypeSSHPassword, Secret: "hunter2"})
	if err != nil {
		t.Fatalf("vault.Create: %v", err)
	}
	return cred.ID
}

func baseInstances() []Instance {
	return []Instance{
		{InstanceID: "i-1", Name: "web-1", PrivateIPs: []string{"10.0.0.1"}, PublicIPs: []string{"1.1.1.1"}, OSType: "linux", Status: "Running"},
		{InstanceID: "i-2", Name: "web-2", PrivateIPs: []string{"10.0.0.2"}, OSType: "linux", Status: "Running"},
		{InstanceID: "i-3", Name: "db-1", PrivateIPs: []string{"10.0.0.3"}, PublicIPs: []string{"3.3.3.3"}, OSType: "linux", Status: "Running"},
	}
}

func TestAccountCRUDRoundtrip(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()

	// 创建:密文落库(不等于明文),列表可见。
	accs, err := env.svc.ListAccounts(ctx)
	if err != nil || len(accs) != 1 {
		t.Fatalf("ListAccounts = %v, %v; want 1", accs, err)
	}
	if string(accs[0].SecretSealed) == "fake-secret-000" {
		t.Fatal("secret 以明文落库")
	}
	if accs[0].AccessKeyID != "LTAI5tFakeFake" || accs[0].Region != "cn-hangzhou" {
		t.Errorf("account = %+v", accs[0])
	}

	// 编辑:名称/Region;secret 留空不轮换。
	name, region := "备用账户", "cn-beijing"
	got := "LTAI5tNewFake"
	updated, err := env.svc.UpdateAccount(ctx, env.account.ID, AccountUpdateInput{Name: &name, AccessKeyID: &got, Region: &region})
	if err != nil {
		t.Fatalf("UpdateAccount: %v", err)
	}
	if updated.Name != name || updated.Region != region || updated.AccessKeyID != got {
		t.Errorf("updated = %+v", updated)
	}

	// 校验:非法 region / 空 secret。
	bad := "CN_Hangzhou!"
	if _, err := env.svc.UpdateAccount(ctx, env.account.ID, AccountUpdateInput{Region: &bad}); !errors.Is(err, ErrInvalidRegion) {
		t.Errorf("非法 region err = %v, want ErrInvalidRegion", err)
	}
	empty := ""
	if _, err := env.svc.UpdateAccount(ctx, env.account.ID, AccountUpdateInput{Secret: &empty}); !errors.Is(err, ErrEmptySecret) {
		t.Errorf("空 secret err = %v, want ErrEmptySecret", err)
	}
	if _, err := env.svc.CreateAccount(ctx, AccountCreateInput{Name: "x", AccessKeyID: "ak", Secret: "", Region: "cn-hangzhou"}); !errors.Is(err, ErrEmptySecret) {
		t.Errorf("create 空 secret err = %v", err)
	}

	// 删除:连带清理实例映射(种子映射须指向真实 server 行,外键约束生效)。
	seedSrv, err := env.target.Create(ctx, target.CreateInput{
		Name: "种子机", Host: "10.9.9.9", User: "root", CredentialID: env.newSSHCred(t, "seed-cred"),
	})
	if err != nil {
		t.Fatalf("seed server: %v", err)
	}
	if _, err := env.db.ExecContext(ctx,
		`INSERT INTO aliyun_ecs_instance_links (account_id, instance_id, server_id, updated_at) VALUES (?, 'i-9', ?, '2026-01-01T00:00:00Z')`,
		env.account.ID, seedSrv.ID); err != nil {
		t.Fatalf("seed link: %v", err)
	}
	if err := env.svc.DeleteAccount(ctx, env.account.ID); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	var n int
	_ = env.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM aliyun_ecs_instance_links`).Scan(&n)
	if n != 0 {
		t.Errorf("删除账户后残留 %d 条映射", n)
	}
	if err := env.svc.DeleteAccount(ctx, env.account.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("重复删除 err = %v, want ErrNotFound", err)
	}
}

func TestVerifyAccount(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	if err := env.svc.VerifyAccount(ctx, env.account.ID); err != nil {
		t.Fatalf("VerifyAccount(成功): %v", err)
	}
	env.fake.verifyErr = errors.New("boom")
	err := env.svc.VerifyAccount(ctx, env.account.ID)
	if !errors.Is(err, ErrVerifyFailed) {
		t.Fatalf("VerifyAccount(失败) err = %v, want ErrVerifyFailed", err)
	}
	if strings.Contains(err.Error(), "fake-secret") {
		t.Errorf("错误泄漏 secret: %v", err)
	}
	if err := env.svc.VerifyAccount(ctx, "no-such"); !errors.Is(err, ErrNotFound) {
		t.Errorf("未知账户 err = %v, want ErrNotFound", err)
	}
}

func TestImportCreateThenUpdate(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	credID := env.newSSHCred(t, "batch-cred")
	env.fake.instances = baseInstances()

	// 首次导入:两台新建,默认内网 IP,统一凭据。
	res, err := env.svc.Import(ctx, ImportInput{
		AccountID:    env.account.ID,
		InstanceIDs:  []string{"i-1", "i-2"},
		CredentialID: credID,
		SSHUser:      "root",
	})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.Total != 2 || res.OK != 2 || res.Failed != 0 {
		t.Fatalf("res = %+v", res)
	}
	srv1, err := env.target.Get(ctx, res.Items[0].ServerID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if srv1.Host != "10.0.0.1" || srv1.User != "root" || srv1.CredentialID != credID || srv1.Port != 22 {
		t.Errorf("srv1 = %+v", srv1)
	}
	if srv1.Name != "web-1" || srv1.Kind != target.KindStandard || srv1.PoolID != target.DefaultPoolID {
		t.Errorf("srv1 放置字段 = %+v", srv1)
	}
	firstID := res.Items[0].ServerID

	// i-1 换了内网 IP 再导入:按实例 ID 映射命中 → 更新原行(IP 刷新,行 ID 不变)。
	env.fake.instances = []Instance{
		{InstanceID: "i-1", Name: "web-1-renamed", PrivateIPs: []string{"10.0.0.11"}, OSType: "linux", Status: "Running"},
	}
	rotated := env.newSSHCred(t, "rotated-cred")
	res2, err := env.svc.Import(ctx, ImportInput{
		AccountID:    env.account.ID,
		InstanceIDs:  []string{"i-1"},
		CredentialID: rotated,
		SSHUser:      "ubuntu",
	})
	if err != nil {
		t.Fatalf("Import#2: %v", err)
	}
	if res2.Items[0].Action != "updated" || res2.Items[0].ServerID != firstID {
		t.Fatalf("res2 = %+v, want updated 到原行 %s", res2, firstID)
	}
	srv1b, _ := env.target.Get(ctx, firstID)
	if srv1b.Host != "10.0.0.11" || srv1b.Name != "web-1-renamed" || srv1b.CredentialID != rotated || srv1b.User != "ubuntu" {
		t.Errorf("srv1b = %+v", srv1b)
	}
}

func TestImportHostFallbackAndLabelsPreserved(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	credID := env.newSSHCred(t, "cred")

	// 手工预登记一台与 i-3 同 IP 的机器(带 labels),无实例映射 → host 兜底应更新它而非新建。
	manual, err := env.target.Create(ctx, target.CreateInput{
		Name: "手动机", Host: "10.0.0.3", User: "admin", CredentialID: credID, Labels: "prod,arch=amd64",
	})
	if err != nil {
		t.Fatalf("manual Create: %v", err)
	}
	env.fake.instances = baseInstances()
	res, err := env.svc.Import(ctx, ImportInput{
		AccountID:    env.account.ID,
		InstanceIDs:  []string{"i-3"},
		CredentialID: credID,
		SSHUser:      "root",
	})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.Items[0].Action != "updated" || res.Items[0].ServerID != manual.ID {
		t.Fatalf("res = %+v, want host 兜底更新手工行 %s", res, manual.ID)
	}
	after, _ := env.target.Get(ctx, manual.ID)
	if after.Labels != "prod,arch=amd64" {
		t.Errorf("更新不应动 labels, got %q", after.Labels)
	}
	if after.Name != "db-1" || after.User != "root" {
		t.Errorf("after = %+v", after)
	}
	// 兜底命中后映射也应写入:再导入走映射而非兜底。
	var linked string
	_ = env.db.QueryRowContext(ctx,
		`SELECT server_id FROM aliyun_ecs_instance_links WHERE account_id = ? AND instance_id = 'i-3'`,
		env.account.ID).Scan(&linked)
	if linked != manual.ID {
		t.Errorf("link = %q, want %q", linked, manual.ID)
	}
}

func TestImportPublicPreferenceAndInlineSecret(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	env.fake.instances = baseInstances()

	// 公网偏好 + 内联密码:自动建凭据,应用到整批。
	res, err := env.svc.Import(ctx, ImportInput{
		AccountID:    env.account.ID,
		InstanceIDs:  []string{"i-1"},
		IPPreference: IPPreferencePublic,
		InlineSecret: &InlineSecret{Kind: inlineSecretPassword, Secret: "pw-batch-1"},
		SSHUser:      "root",
	})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	srv, _ := env.target.Get(ctx, res.Items[0].ServerID)
	if srv.Host != "1.1.1.1" {
		t.Errorf("公网偏好 host = %q, want 1.1.1.1", srv.Host)
	}
	creds, err := env.vault.List()
	if err != nil {
		t.Fatalf("vault.List: %v", err)
	}
	var found bool
	for _, c := range creds {
		if c.Type == vault.TypeSSHPassword && strings.Contains(c.Name, "阿里云 ECS") {
			found = true
		}
	}
	if !found {
		t.Errorf("内联凭据未入 vault: %+v", creds)
	}
	if srv.CredentialID == "" {
		t.Error("内联凭据未应用到机器行")
	}

	// 无公网 IP 的实例在公网偏好下 → 逐台失败,整批不 500。
	res2, err := env.svc.Import(ctx, ImportInput{
		AccountID:    env.account.ID,
		InstanceIDs:  []string{"i-2"},
		IPPreference: IPPreferencePublic,
		CredentialID: env.newSSHCred(t, "c2"),
		SSHUser:      "root",
	})
	if err != nil {
		t.Fatalf("Import#2: %v", err)
	}
	if res2.Failed != 1 || res2.Items[0].OK {
		t.Fatalf("res2 = %+v, want 1 failed", res2)
	}
	if !strings.Contains(res2.Items[0].Err, "公网") {
		t.Errorf("err = %q, want 公网 IP 缺失人话", res2.Items[0].Err)
	}
}

func TestImportPerItemTolerance(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	credID := env.newSSHCred(t, "cred")
	env.fake.instances = baseInstances()

	res, err := env.svc.Import(ctx, ImportInput{
		AccountID:    env.account.ID,
		InstanceIDs:  []string{"i-1", "i-gone", "i-2"},
		CredentialID: credID,
		SSHUser:      "root",
	})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.Total != 3 || res.OK != 2 || res.Failed != 1 {
		t.Fatalf("res = %+v", res)
	}
	if res.Items[1].OK || res.Items[1].Err == "" {
		t.Errorf("已释放实例应逐台失败: %+v", res.Items[1])
	}

	// 无效凭据引用:逐台失败并给人话(target 层校验)。
	res2, _ := env.svc.Import(ctx, ImportInput{
		AccountID:    env.account.ID,
		InstanceIDs:  []string{"i-1"},
		CredentialID: "no-such-cred",
		SSHUser:      "root",
	})
	if res2.OK != 0 || res2.Failed != 1 {
		t.Fatalf("res2 = %+v", res2)
	}
	if !strings.Contains(res2.Items[0].Err, "凭据") {
		t.Errorf("err = %q, want 凭据相关人话", res2.Items[0].Err)
	}
}

func TestImportValidation(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	credID := env.newSSHCred(t, "cred")

	cases := []struct {
		name string
		in   ImportInput
		want error
	}{
		{"空实例集", ImportInput{AccountID: env.account.ID, CredentialID: credID, SSHUser: "root"}, ErrNoInstances},
		{"未知账户", ImportInput{AccountID: "nope", InstanceIDs: []string{"i-1"}, CredentialID: credID, SSHUser: "root"}, ErrNotFound},
		{"双认证", ImportInput{AccountID: env.account.ID, InstanceIDs: []string{"i-1"}, CredentialID: credID, InlineSecret: &InlineSecret{Kind: "password", Secret: "x"}, SSHUser: "root"}, ErrInvalidCredential},
		{"无认证", ImportInput{AccountID: env.account.ID, InstanceIDs: []string{"i-1"}, SSHUser: "root"}, ErrInvalidCredential},
		{"内联空 secret", ImportInput{AccountID: env.account.ID, InstanceIDs: []string{"i-1"}, InlineSecret: &InlineSecret{Kind: "password"}, SSHUser: "root"}, ErrInvalidCredential},
		{"内联坏私钥", ImportInput{AccountID: env.account.ID, InstanceIDs: []string{"i-1"}, InlineSecret: &InlineSecret{Kind: "key", Secret: "not a pem"}, SSHUser: "root"}, ErrInvalidCredential},
		{"非法 IP 偏好", ImportInput{AccountID: env.account.ID, InstanceIDs: []string{"i-1"}, CredentialID: credID, IPPreference: "both", SSHUser: "root"}, ErrInvalidIPPreference},
		{"空用户", ImportInput{AccountID: env.account.ID, InstanceIDs: []string{"i-1"}, CredentialID: credID}, ErrInvalidSSHUser},
	}
	for _, tc := range cases {
		if _, err := env.svc.Import(ctx, tc.in); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}

	// 超批量上限。
	many := make([]string, MaxImportBatch+1)
	for i := range many {
		many[i] = fmt.Sprintf("i-x-%d", i)
	}
	if _, err := env.svc.Import(ctx, ImportInput{AccountID: env.account.ID, InstanceIDs: many, CredentialID: credID, SSHUser: "root"}); !errors.Is(err, ErrTooManyInstances) {
		t.Errorf("超上限 err = %v, want ErrTooManyInstances", err)
	}

	// 拉取实例失败 → 整批报错。
	env.fake.listErr = errors.New("net down")
	if _, err := env.svc.Import(ctx, ImportInput{AccountID: env.account.ID, InstanceIDs: []string{"i-1"}, CredentialID: credID, SSHUser: "root"}); !errors.Is(err, ErrListFailed) {
		t.Errorf("listErr err = %v, want ErrListFailed", err)
	}
}
