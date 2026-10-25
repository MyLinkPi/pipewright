package certdeploy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/store"
	"github.com/huangchengsir/pipewright/internal/storetest"
	"github.com/huangchengsir/pipewright/internal/target"
)

// ---------- fakes ----------

// fakeExecutor 记录 Exec/Upload 调用;failOn 非空时命令含该子串 → 非零退出。
type fakeExecutor struct {
	mu      sync.Mutex
	servers map[string]*target.Server
	execs   [][]string
	uploads []string
	failOn  string
}

func (f *fakeExecutor) Exec(ctx context.Context, serverID string, cmd []string) (*target.ExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execs = append(f.execs, cmd)
	if f.failOn != "" && strings.Contains(strings.Join(cmd, " "), f.failOn) {
		return &target.ExecResult{ExitCode: 1, Stderr: "fake failure"}, nil
	}
	return &target.ExecResult{ExitCode: 0}, nil
}

func (f *fakeExecutor) Upload(ctx context.Context, serverID string, content io.Reader, remotePath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads = append(f.uploads, remotePath)
	return nil
}

func (f *fakeExecutor) Get(ctx context.Context, id string) (*target.Server, error) {
	if sv, ok := f.servers[id]; ok {
		return sv, nil
	}
	return nil, target.ErrNotFound
}

func (f *fakeExecutor) snapshotExecs() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.execs))
	copy(out, f.execs)
	return out
}

// fakeCertSource 返回固定 PEM(ErrPEM 非空时报错)。
type fakeCertSource struct{ err error }

func (f fakeCertSource) OpenCertPEM(ctx context.Context, certID string) (string, string, error) {
	if f.err != nil {
		return "", "", f.err
	}
	return "-----BEGIN CERTIFICATE-----\nFAKE\n", "-----BEGIN PRIVATE KEY-----\nFAKE\n", nil
}

// ---------- seed ----------

func seedCert(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(
		`INSERT INTO certificates (id, primary_domain, status, created_at, updated_at)
		 VALUES (?, ?, 'issued', ?, ?)`, id, id+".example.com", now, now); err != nil {
		t.Fatalf("seed cert: %v", err)
	}
}

func seedServer(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	// servers.credential_id NOT NULL 且 FK → credentials:先落一条占位凭据(密文列只占位)。
	if _, err := db.Exec(
		`INSERT INTO credentials (id, name, type, scope, ciphertext, masked_value, created_at, updated_at)
		 VALUES (?, ?, 'ssh_key', '', x'00', 'masked', ?, ?)`, "cred-"+id, "cred-"+id, now, now); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO servers (id, name, host, port, user, credential_id, jumps, labels, max_builds, priority, gpu, shared_build, pool_id, kind, created_at, updated_at)
		 VALUES (?, ?, '10.0.0.1', 22, 'root', ?, '[]', '', 0, 0, 0, 0, 'default', 'standard', ?, ?)`,
		id, "srv-"+id, "cred-"+id, now, now); err != nil {
		t.Fatalf("seed server: %v", err)
	}
}

func newTestService(t *testing.T, db *sql.DB, user string) (*Service, *fakeExecutor) {
	t.Helper()
	fe := &fakeExecutor{servers: map[string]*target.Server{
		"srv-1": {ID: "srv-1", Name: "srv-1", User: user},
	}}
	return New(db, fe, fakeCertSource{}), fe
}

// ---------- CRUD / 校验 / 占用 ----------

// TestTargetCRUD 验证 部署目标 创建 → 查询/列表/筛选 → 更新 → 占用判定 → 删除(含历史级联)
// 在真库上往返一致(两方言)。
func TestTargetCRUD(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		ctx := context.Background()
		seedCert(t, st.DB, "cert-1")
		seedServer(t, st.DB, "srv-1")
		svc, _ := newTestService(t, st.DB, "root")

		d, err := svc.Create(ctx, CreateInput{
			CertID: "cert-1", ServerID: "srv-1", Name: " 网关机 ",
			CertPath: "/etc/nginx/certs/fullchain.pem", KeyPath: "/etc/nginx/certs/privkey.pem",
			PostCommand: "nginx -s reload",
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if d.Name != "网关机" || !d.Enabled || d.Status != "" || d.ServerName != "srv-srv-1" || d.CertPrimaryDomain != "cert-1.example.com" {
			t.Fatalf("创建回显不符: %+v", d)
		}

		// 列表按证书筛选。
		items, err := svc.List(ctx, "cert-1")
		if err != nil || len(items) != 1 {
			t.Fatalf("list: %v, n=%d", err, len(items))
		}
		if _, err := svc.List(ctx, "cert-none"); err != nil || len(items) != 1 {
			t.Fatalf("按不存在证书筛选应空: %v", err)
		}

		// 更新(路径/命令/开关/名)。
		no := false
		d, err = svc.Update(ctx, d.ID, UpdateInput{Name: strPtr("新名"), Enabled: &no, PostCommand: strPtr("")})
		if err != nil || d.Name != "新名" || d.Enabled || d.PostCommand != "" {
			t.Fatalf("update: %v, %+v", err, d)
		}

		// 占用判定(停用目标仍占用)。
		if used, _ := svc.UsesCert(ctx, "cert-1"); !used {
			t.Fatal("停用目标也应占用证书")
		}
		if used, _ := svc.UsesCert(ctx, "cert-none"); used {
			t.Fatal("未引用证书不应占用")
		}

		// 删除(含历史)。
		if err := svc.Delete(ctx, d.ID); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if err := svc.Delete(ctx, d.ID); err != ErrNotFound {
			t.Fatalf("重复删除应 ErrNotFound, got %v", err)
		}
	})
}

// TestCreateValidation 验证创建校验:相对路径 / 路径相同 / 证书不存在 / 服务器不存在。
func TestCreateValidation(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		ctx := context.Background()
		seedCert(t, st.DB, "cert-1")
		seedServer(t, st.DB, "srv-1")
		svc, _ := newTestService(t, st.DB, "root")
		base := CreateInput{CertID: "cert-1", ServerID: "srv-1", CertPath: "/a/c.pem", KeyPath: "/a/k.pem"}

		if _, err := svc.Create(ctx, CreateInput{CertID: "cert-1", ServerID: "srv-1", CertPath: "a/c.pem", KeyPath: "/a/k.pem"}); err == nil || !strings.Contains(err.Error(), "绝对路径") {
			t.Fatalf("相对路径应拒绝: %v", err)
		}
		if _, err := svc.Create(ctx, CreateInput{CertID: "cert-1", ServerID: "srv-1", CertPath: "/a/same", KeyPath: "/a/same"}); err == nil || !strings.Contains(err.Error(), "不能相同") {
			t.Fatalf("相同路径应拒绝: %v", err)
		}
		if _, err := svc.Create(ctx, CreateInput{CertID: "cert-none", ServerID: "srv-1", CertPath: "/a/c.pem", KeyPath: "/a/k.pem"}); err != ErrCertNotFound {
			t.Fatalf("证书不存在应 ErrCertNotFound, got %v", err)
		}
		if _, err := svc.Create(ctx, CreateInput{CertID: "cert-1", ServerID: "srv-none", CertPath: "/a/c.pem", KeyPath: "/a/k.pem"}); err == nil {
			t.Fatal("服务器不存在应报错")
		}
		if _, err := svc.Create(ctx, base); err != nil {
			t.Fatalf("合法输入应通过: %v", err)
		}
	})
}

// ---------- 部署编排 ----------

// TestDeployOneSuccess 验证 root 直跑的成功序列:清残留 → 上传×2 → 落位(644/600)→
// 部署后命令;状态/时间/历史回写。
func TestDeployOneSuccess(t *testing.T) {
	db := storetest.OpenDB(t)
	ctx := context.Background()
	seedCert(t, db, "cert-1")
	seedServer(t, db, "srv-1")
	svc, fe := newTestService(t, db, "root")

	d, err := svc.Create(ctx, CreateInput{
		CertID: "cert-1", ServerID: "srv-1",
		CertPath: "/etc/nginx/certs/fullchain.pem", KeyPath: "/etc/nginx/certs/privkey.pem",
		PostCommand: "nginx -s reload",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := svc.DeployOne(ctx, d.ID)
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if got.Status != StatusSuccess || got.StatusDetail != "" || got.LastDeployedAt.IsZero() {
		t.Fatalf("成功状态不符: %+v", got)
	}

	execs := fe.snapshotExecs()
	// 预期命令序列:rm 残留 → 落位证书(sh -c mv…chmod 644)→ 落位私钥(600)→ 部署后命令(sh -c nginx)。
	if len(execs) != 4 {
		t.Fatalf("命令数应 4, got %d: %q", len(execs), execs)
	}
	if strings.Join(execs[0], " ") != "rm -f /tmp/pw-certdeploy-"+d.ID+"-cert.tmp /tmp/pw-certdeploy-"+d.ID+"-key.tmp" {
		t.Fatalf("首条应为清残留: %q", execs[0])
	}
	instalCert, installKey := strings.Join(execs[1], " "), strings.Join(execs[2], " ")
	if !strings.Contains(instalCert, "chmod \"$2\" \"$1\"") || !strings.Contains(instalCert, "644") || !strings.Contains(instalCert, "/etc/nginx/certs/fullchain.pem") {
		t.Fatalf("落位证书命令不符: %q", execs[1])
	}
	if !strings.Contains(installKey, "600") || !strings.Contains(installKey, "/etc/nginx/certs/privkey.pem") {
		t.Fatalf("落位私钥命令不符: %q", execs[2])
	}
	if strings.Join(execs[3], " ") != "sh -c nginx -s reload" {
		t.Fatalf("部署后命令不符: %q", execs[3])
	}
	if len(fe.uploads) != 2 || !strings.Contains(fe.uploads[0], d.ID+"-cert.tmp") || !strings.Contains(fe.uploads[1], d.ID+"-key.tmp") {
		t.Fatalf("上传序列不符: %q", fe.uploads)
	}

	runs, err := svc.ListRuns(ctx, d.ID, 0)
	if err != nil || len(runs) != 1 || runs[0].Status != StatusSuccess || runs[0].Source != SourceManual {
		t.Fatalf("历史不符: %v %+v", err, runs)
	}
}

// TestDeployOneFailures 验证失败路径:落位失败 / 部署后命令失败 / 证书未就绪,
// 均落 status=failed + detail 人话 + 历史。
func TestDeployOneFailures(t *testing.T) {
	db := storetest.OpenDB(t)
	ctx := context.Background()
	seedCert(t, db, "cert-1")
	seedServer(t, db, "srv-1")

	mk := func(post string) *Target {
		svc, _ := newTestService(t, db, "root")
		d, err := svc.Create(ctx, CreateInput{
			CertID: "cert-1", ServerID: "srv-1", CertPath: "/a/c.pem", KeyPath: "/a/k.pem", PostCommand: post,
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		return d
	}

	// 证书源报错。
	svcBad, _ := newTestService(t, db, "root")
	dBad, err := svcBad.Create(ctx, CreateInput{CertID: "cert-1", ServerID: "srv-1", CertPath: "/a/c.pem", KeyPath: "/a/k.pem"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	svcBad.certs = fakeCertSource{err: context.DeadlineExceeded}
	got, err := svcBad.DeployOne(ctx, dBad.ID)
	if err == nil || got.Status != StatusFailed || !strings.Contains(got.StatusDetail, "证书未就绪") {
		t.Fatalf("证书未就绪应 failed: err=%v %+v", err, got)
	}

	// 部署后命令失败。
	svc, fe := newTestService(t, db, "root")
	d := mk("nginx -s reload")
	fe.failOn = "nginx"
	got, err = svc.DeployOne(ctx, d.ID)
	if err == nil || got.Status != StatusFailed || !strings.Contains(got.StatusDetail, "部署后命令") {
		t.Fatalf("部署后命令失败应 failed: err=%v %+v", err, got)
	}
	runs, _ := svc.ListRuns(ctx, d.ID, 0)
	if len(runs) != 1 || runs[0].Status != StatusFailed {
		t.Fatalf("失败历史不符: %+v", runs)
	}

	// 落位失败(mv 阶段)。
	svc2, fe2 := newTestService(t, db, "root")
	d2, err := svc2.Create(ctx, CreateInput{CertID: "cert-1", ServerID: "srv-1", CertPath: "/a/c.pem", KeyPath: "/a/k.pem"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	fe2.failOn = "mv "
	got2, err2 := svc2.DeployOne(ctx, d2.ID)
	if err2 == nil || got2.Status != StatusFailed || !strings.Contains(got2.StatusDetail, "落位证书失败") {
		t.Fatalf("落位失败应 failed: err=%v %+v", err2, got2)
	}
}

// TestDeployOneSudoPrefix 验证提权探测:非 root 且免密 sudo 可用 → 落位/命令加 sudo -n
// 前缀;不可用 → 直跑。
func TestDeployOneSudoPrefix(t *testing.T) {
	db := storetest.OpenDB(t)
	ctx := context.Background()
	seedCert(t, db, "cert-1")
	seedServer(t, db, "srv-1")

	for _, tc := range []struct {
		user, wantFirst string
	}{
		{"root", "sh -c"},           // root:探测都省了,首条即清残留直跑
		{"ubuntu", "sudo -n sh -c"}, // 免密 sudo 可用(fake 全部 exit 0)
	} {
		svc, fe := newTestService(t, db, tc.user)
		d, err := svc.Create(ctx, CreateInput{CertID: "cert-1", ServerID: "srv-1", CertPath: "/a/c.pem", KeyPath: "/a/k.pem"})
		if err != nil {
			t.Fatalf("create(%s): %v", tc.user, err)
		}
		if _, err := svc.DeployOne(ctx, d.ID); err != nil {
			t.Fatalf("deploy(%s): %v", tc.user, err)
		}
		execs := fe.snapshotExecs()
		if len(execs) == 0 {
			t.Fatalf("user=%s 无命令记录", tc.user)
		}
		first := strings.Join(execs[0], " ")
		if tc.user != "root" && first != "sudo -n true" {
			t.Fatalf("user=%s 首条应为 sudo 探测, got %q", tc.user, first)
		}
		if tc.user == "root" && !strings.HasPrefix(first, "rm -f") {
			t.Fatalf("root 首条应为清残留, got %q", first)
		}
		// 落位命令(含 mkdir/mv/chmod 模板)按用户带/不带 sudo -n 前缀。
		for _, c := range execs {
			joined := strings.Join(c, " ")
			if strings.Contains(joined, "chmod") {
				want := tc.wantFirst
				if !strings.HasPrefix(joined, want) {
					t.Fatalf("user=%s 落位命令前缀应 %q: %q", tc.user, want, joined)
				}
			}
		}
	}
}

// TestDeployAllForCert 验证联动扇出:只部署启用目标;失败聚合人话错误;单目标状态独立。
func TestDeployAllForCert(t *testing.T) {
	db := storetest.OpenDB(t)
	ctx := context.Background()
	seedCert(t, db, "cert-1")
	seedServer(t, db, "srv-1")
	svc, fe := newTestService(t, db, "root")

	on := true
	d1, _ := svc.Create(ctx, CreateInput{CertID: "cert-1", ServerID: "srv-1", CertPath: "/a/c.pem", KeyPath: "/a/k.pem"})
	d2, _ := svc.Create(ctx, CreateInput{CertID: "cert-1", ServerID: "srv-1", CertPath: "/b/c.pem", KeyPath: "/b/k.pem"})
	_, err := svc.Create(ctx, CreateInput{CertID: "cert-1", ServerID: "srv-1", CertPath: "/c/c.pem", KeyPath: "/c/k.pem", Enabled: &on})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	off := false
	d3, _ := svc.Create(ctx, CreateInput{CertID: "cert-1", ServerID: "srv-1", CertPath: "/d/c.pem", KeyPath: "/d/k.pem", Enabled: &off})

	if err := svc.RedeployCert(ctx, "cert-1"); err != nil {
		t.Fatalf("全成功应 nil: %v", err)
	}
	got3, _ := svc.Get(ctx, d3.ID)
	if got3.Status != "" {
		t.Fatal("停用目标不应被联动部署")
	}

	fe.mu.Lock()
	fe.failOn = "/b/"
	fe.mu.Unlock()
	err = svc.DeployAllForCert(ctx, "cert-1", SourceRenew)
	if err == nil || !strings.Contains(err.Error(), "1/3") {
		t.Fatalf("单目标失败应聚合错误: %v", err)
	}
	got1, _ := svc.Get(ctx, d1.ID)
	got2, _ := svc.Get(ctx, d2.ID)
	if got1.Status != StatusSuccess || got2.Status != StatusFailed {
		t.Fatalf("目标状态应独立: d1=%s d2=%s", got1.Status, got2.Status)
	}
	runs, _ := svc.ListRuns(ctx, d2.ID, 0)
	if len(runs) == 0 || runs[0].Source != SourceRenew {
		t.Fatalf("联动历史应记 source=renew: %+v", runs)
	}
}

// TestPruneHistory 验证每目标历史保留窗口(HistoryKeep=50):超额后旧记录被裁。
func TestPruneHistory(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		ctx := context.Background()
		seedCert(t, st.DB, "cert-1")
		seedServer(t, st.DB, "srv-1")
		svc, _ := newTestService(t, st.DB, "root")
		d, err := svc.Create(ctx, CreateInput{CertID: "cert-1", ServerID: "srv-1", CertPath: "/a/c.pem", KeyPath: "/a/k.pem"})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		now := time.Now().UTC()
		for i := 0; i < HistoryKeep+7; i++ {
			svc.recordRun(ctx, &Run{
				ID: fmt.Sprintf("run-%d", i), DeployID: d.ID, ServerID: "srv-1", Source: SourceManual,
				Status: StatusSuccess, StartedAt: now.Add(time.Duration(i) * time.Second),
				FinishedAt: now.Add(time.Duration(i) * time.Second),
			})
		}
		var n int
		if err := st.DB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM cert_deploy_runs WHERE deploy_id = ?`, d.ID).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != HistoryKeep {
			t.Fatalf("历史应裁到 %d, got %d", HistoryKeep, n)
		}
		// 删除目标后历史一并清除。
		if err := svc.Delete(ctx, d.ID); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if err := st.DB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM cert_deploy_runs`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("删除目标后历史应清空: n=%d err=%v", n, err)
		}
	})
}

func strPtr(s string) *string { return &s }

// ---------- safeline 目标(经 WAF API 推证书) ----------

// fakeWafGateway 记录推送参数;可切换失败与宿主解析。
type fakeWafGateway struct {
	pushes   []string // instanceID|certName
	failWith error
	serverID string // WafInstanceServerID 返回值
}

func (f *fakeWafGateway) UpsertCert(ctx context.Context, instanceID, name, certPEM, keyPEM string) error {
	if f.failWith != nil {
		return f.failWith
	}
	f.pushes = append(f.pushes, instanceID+"|"+name)
	return nil
}

func (f *fakeWafGateway) WafInstanceServerID(ctx context.Context, instanceID string) (string, error) {
	if f.serverID == "" {
		return "", nil
	}
	return f.serverID, nil
}

func TestSafelineTargetLifecycle(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		ctx := context.Background()
		seedCert(t, st.DB, "cert-1")
		seedServer(t, st.DB, "srv-1")
		svc, fe := newTestService(t, st.DB, "root")
		waf := &fakeWafGateway{serverID: "srv-1"}
		svc.WithWafGateway(waf)

		// 未注入 gateway → 创建 safeline 目标被拒(人话)。
		svcNoWaf, _ := newTestService(t, st.DB, "root")
		if _, err := svcNoWaf.Create(ctx, CreateInput{Kind: KindSafeline, CertID: "cert-1", WafInstanceID: "w1", CertName: "a.com"}); err == nil || !strings.Contains(err.Error(), "未装配") {
			t.Fatalf("未装配应报错,得 %v", err)
		}

		// 创建:serverID 由实例解析;路径/命令字段被清空;ssh 校验不适用。
		d, err := svc.Create(ctx, CreateInput{
			Kind: KindSafeline, CertID: "cert-1", WafInstanceID: "w1", CertName: " a.com ",
			CertPath: "/tmp/should-be-ignored", Name: "雷池",
		})
		if err != nil {
			t.Fatalf("create safeline: %v", err)
		}
		if d.Kind != KindSafeline || d.WafInstanceID != "w1" || d.CertName != "a.com" || d.ServerID != "srv-1" || d.CertPath != "" {
			t.Fatalf("safeline 目标不符:%+v", d)
		}
		// 实例未登记宿主 → 人话拒绝。
		wafNoHost := &fakeWafGateway{serverID: ""}
		svc.WithWafGateway(wafNoHost)
		if _, err := svc.Create(ctx, CreateInput{Kind: KindSafeline, CertID: "cert-1", WafInstanceID: "w2", CertName: "b.com"}); err == nil || !strings.Contains(err.Error(), "部署宿主") {
			t.Fatalf("缺宿主应报错,得 %v", err)
		}
		svc.WithWafGateway(waf)

		// 部署:走 WAF API(无 SSH 执行),证书名随目标;失败人话落状态。
		svc.WithWafGateway(waf)
		if _, err := svc.DeployOne(ctx, d.ID); err != nil {
			t.Fatalf("deploy: %v", err)
		}
		got, _ := svc.Get(ctx, d.ID)
		if got.Status != StatusSuccess || len(waf.pushes) != 1 || waf.pushes[0] != "w1|a.com" {
			t.Fatalf("部署结果不符:%s %+v", got.Status, waf.pushes)
		}
		if n := len(fe.execs); n != 0 {
			t.Fatalf("safeline 部署不应产生 SSH 执行:%d", n)
		}
		waf.failWith = errors.New("boom token")
		if _, err := svc.DeployOne(ctx, d.ID); err == nil || !strings.Contains(err.Error(), "SafeLine") {
			t.Fatalf("失败路径应返回人话错误,得 %v", err)
		}
		got, _ = svc.Get(ctx, d.ID)
		if got.Status != StatusFailed || !strings.Contains(got.StatusDetail, "SafeLine") {
			t.Fatalf("失败状态不符:%+v", got)
		}
		waf.failWith = nil

		// 更新:certName 可改,空名拒绝;路径字段对 safeline 不校验。
		name := "new.com"
		d2, err := svc.Update(ctx, d.ID, UpdateInput{CertName: &name, CertPath: strPtr("")})
		if err != nil || d2.CertName != "new.com" {
			t.Fatalf("更新证书名:%v %+v", err, d2)
		}
		empty := ""
		if _, err := svc.Update(ctx, d.ID, UpdateInput{CertName: &empty}); err == nil {
			t.Fatal("空证书名应拒绝")
		}
	})
}
