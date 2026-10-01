package platformhttps

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/store"
	"github.com/huangchengsir/pipewright/internal/storetest"
	"github.com/huangchengsir/pipewright/internal/target"
)

// --- 测试桩(照 servicereg 包手法) ---

// fakeTarget 是捕获 Exec/Upload 的假 target.Service;resultFor 可按命令定制结果。
type fakeTarget struct {
	execCalls   [][]string
	uploads     []string
	uploadBytes map[string]string
	resultFor   func(cmd []string) *target.ExecResult
}

func (f *fakeTarget) Exec(_ context.Context, _ string, cmd []string) (*target.ExecResult, error) {
	f.execCalls = append(f.execCalls, cmd)
	if f.resultFor != nil {
		if r := f.resultFor(cmd); r != nil {
			return r, nil
		}
	}
	return &target.ExecResult{ExitCode: 0}, nil
}

func (f *fakeTarget) Upload(_ context.Context, _ string, content io.Reader, remotePath string) error {
	f.uploads = append(f.uploads, remotePath)
	b, _ := io.ReadAll(content)
	if f.uploadBytes == nil {
		f.uploadBytes = map[string]string{}
	}
	f.uploadBytes[remotePath] = string(b)
	return nil
}

func (f *fakeTarget) Get(context.Context, string) (*target.Server, error) { return nil, nil }
func (f *fakeTarget) List(context.Context) ([]*target.Server, error)      { return nil, nil }
func (f *fakeTarget) Create(context.Context, target.CreateInput) (*target.Server, error) {
	return nil, nil
}
func (f *fakeTarget) Update(context.Context, string, target.UpdateInput) (*target.Server, error) {
	return nil, nil
}
func (f *fakeTarget) Delete(context.Context, string) error                     { return nil }
func (f *fakeTarget) Test(context.Context, string) (*target.TestResult, error) { return nil, nil }
func (f *fakeTarget) ExecStream(context.Context, string, []string) (io.ReadCloser, error) {
	return nil, nil
}
func (f *fakeTarget) ExecInteractive(context.Context, string, []string) (target.Session, error) {
	return nil, nil
}

func hasCmd(f *fakeTarget, prefix ...string) bool {
	joined := strings.Join(prefix, " ")
	for _, c := range f.execCalls {
		if strings.HasPrefix(strings.Join(c, " "), joined) {
			return true
		}
	}
	return false
}

// fakeCertSource 是注入的假证书源(元数据 + PEM)。
type fakeCertSource struct {
	certs map[string]*CertInfo
	pems  map[string][2]string
}

var errFakeCertNotFound = errors.New("fake: cert not found")

func (f *fakeCertSource) GetCert(_ context.Context, id string) (*CertInfo, error) {
	c, ok := f.certs[id]
	if !ok {
		return nil, errFakeCertNotFound
	}
	return c, nil
}

func (f *fakeCertSource) OpenCertPEM(_ context.Context, id string) (string, string, error) {
	p, ok := f.pems[id]
	if !ok {
		return "", "", errFakeCertNotFound
	}
	return p[0], p[1], nil
}

func newFakeCerts() *fakeCertSource {
	return &fakeCertSource{
		certs: map[string]*CertInfo{
			"c1": {ID: "c1", PrimaryDomain: "*.efg.com", Domains: []string{"*.efg.com", "efg.com"}, Status: "issued"},
			"c2": {ID: "c2", PrimaryDomain: "other.com", Domains: []string{"other.com"}, Status: "issued"},
			"c3": {ID: "c3", PrimaryDomain: "efg.com", Domains: []string{"efg.com"}, Status: "pending"},
			"c4": {ID: "c4", PrimaryDomain: "pip.efg.com", Domains: []string{"pip.efg.com"}, Status: "issued"},
		},
		pems: map[string][2]string{
			"c1": {"-----BEGIN CERTIFICATE-----\nfull\n-----END CERTIFICATE-----\n", "-----BEGIN PRIVATE KEY-----\nkey\n-----END PRIVATE KEY-----\n"},
			"c4": {"-----BEGIN CERTIFICATE-----\nfull4\n-----END CERTIFICATE-----\n", "-----BEGIN PRIVATE KEY-----\nkey4\n-----END PRIVATE KEY-----\n"},
		},
	}
}

func newTestService(t *testing.T, db *sql.DB) (Service, *fakeTarget, *fakeCertSource) {
	t.Helper()
	ft := &fakeTarget{}
	cs := newFakeCerts()
	return New(db, ft, cs, 8080), ft, cs
}

// nginxOK 让 fakeTarget 表现为一台装好 nginx 的 root 机器(nginx -T 输出含平台标记;
// 匹配对 sudo -n 前缀免疫)。
func nginxOK(f *fakeTarget) {
	f.resultFor = func(cmd []string) *target.ExecResult {
		joined := strings.Join(cmd, " ")
		trimmed := strings.TrimPrefix(joined, "sudo -n ")
		switch {
		case joined == "id -u":
			return &target.ExecResult{ExitCode: 0, Stdout: "0"}
		case trimmed == "nginx -T":
			return &target.ExecResult{ExitCode: 0, Stdout: "# " + confMarker + " ..."}
		case trimmed == "nginx -v":
			return &target.ExecResult{ExitCode: 0, Stderr: "nginx version: nginx/1.24.0"}
		}
		return nil // 其余走默认 exit 0
	}
}

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }
func intPtr(i int) *int       { return &i }

func enableSettings(t *testing.T, svc Service) {
	t.Helper()
	if _, err := svc.SaveSettings(context.Background(), SettingsInput{
		Enabled: boolPtr(true), ServerID: strPtr("srv-1"), Domain: strPtr("pip.efg.com"),
		CertID: strPtr("c1"), HTTPRedirect: boolPtr(true),
	}); err != nil {
		t.Fatalf("保存设置:%v", err)
	}
}

// --- 校验 ---

func TestSaveSettingsValidation(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc, _, _ := newTestService(t, st.DB)
		ctx := context.Background()

		// 未启用:允许保存部分配置(宽松校验),但域名/上游形态仍查。
		if _, err := svc.SaveSettings(ctx, SettingsInput{Domain: strPtr("bad_domain")}); err == nil ||
			!errors.Is(err, ErrInvalidDomain) {
			t.Fatalf("非法域名应 ErrInvalidDomain,得 %v", err)
		}
		if _, err := svc.SaveSettings(ctx, SettingsInput{UpstreamHost: strPtr("bad host!")}); err == nil ||
			!errors.Is(err, ErrInvalidUpstream) {
			t.Fatalf("非法上游应 ErrInvalidUpstream,得 %v", err)
		}
		if _, err := svc.SaveSettings(ctx, SettingsInput{UpstreamPort: intPtr(70000)}); err == nil ||
			!errors.Is(err, ErrInvalidUpstream) {
			t.Fatalf("越界端口应 ErrInvalidUpstream,得 %v", err)
		}
		if _, err := svc.SaveSettings(ctx, SettingsInput{Domain: strPtr("pip.efg.com")}); err != nil {
			t.Fatalf("未启用的部分配置应可保存:%v", err)
		}

		// 启用但缺项 → ErrNotConfigured。
		_, err := svc.SaveSettings(ctx, SettingsInput{Enabled: boolPtr(true), ServerID: strPtr("srv-1")})
		if err == nil || !errors.Is(err, ErrNotConfigured) {
			t.Fatalf("启用缺域名/证书应 ErrNotConfigured,得 %v", err)
		}
		// 证书未签发完成。
		_, err = svc.SaveSettings(ctx, SettingsInput{
			Enabled: boolPtr(true), ServerID: strPtr("srv-1"), Domain: strPtr("efg.com"), CertID: strPtr("c3"),
		})
		if err == nil || !errors.Is(err, ErrCertNotReady) {
			t.Fatalf("未签发证书应 ErrCertNotReady,得 %v", err)
		}
		// 证书不覆盖域名(c2 = other.com)。
		_, err = svc.SaveSettings(ctx, SettingsInput{
			Enabled: boolPtr(true), ServerID: strPtr("srv-1"), Domain: strPtr("pip.efg.com"), CertID: strPtr("c2"),
		})
		if err == nil || !errors.Is(err, ErrCertNotCover) {
			t.Fatalf("不覆盖证书应 ErrCertNotCover,得 %v", err)
		}
		// 证书不存在。
		_, err = svc.SaveSettings(ctx, SettingsInput{
			Enabled: boolPtr(true), ServerID: strPtr("srv-1"), Domain: strPtr("pip.efg.com"), CertID: strPtr("c9"),
		})
		if err == nil || !errors.Is(err, ErrCertNotReady) {
			t.Fatalf("不存在证书应 ErrCertNotReady,得 %v", err)
		}
		// 通配符证书覆盖子域 + 完整配置 → 通过。
		if _, err := svc.SaveSettings(ctx, SettingsInput{
			Enabled: boolPtr(true), ServerID: strPtr("srv-1"), Domain: strPtr("pip.efg.com"), CertID: strPtr("c1"),
		}); err != nil {
			t.Fatalf("完整配置应可保存:%v", err)
		}
		// 证书模块未接入时启用 → ErrCertSourceMissing。
		svc2 := New(st.DB, &fakeTarget{}, nil, 8080)
		_, err = svc2.SaveSettings(ctx, SettingsInput{
			Enabled: boolPtr(true), ServerID: strPtr("srv-1"), Domain: strPtr("pip.efg.com"), CertID: strPtr("c1"),
		})
		if err == nil || !errors.Is(err, ErrCertSourceMissing) {
			t.Fatalf("无证书源应 ErrCertSourceMissing,得 %v", err)
		}
	})
}

// --- 应用 ---

func TestApplySuccess(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc, ft, _ := newTestService(t, st.DB)
		nginxOK(ft)
		ctx := context.Background()
		enableSettings(t, svc)

		s, err := svc.Apply(ctx)
		if err != nil {
			t.Fatalf("应用:%v", err)
		}
		if s.Status != StatusActive || s.StatusDetail != "" {
			t.Fatalf("应用后应 active,得 %q/%q", s.Status, s.StatusDetail)
		}
		// 三份临时文件(配置/证书/私钥)上传。
		for _, p := range []string{tmpConfPath, tmpCertPath, tmpKeyPath} {
			if _, ok := ft.uploadBytes[p]; !ok {
				t.Fatalf("缺少上传 %s:%v", p, ft.uploads)
			}
		}
		// 上传的配置含标记 + 域名 + 默认上游。
		conf := ft.uploadBytes[tmpConfPath]
		for _, want := range []string{confMarker, "pip.efg.com", "http://127.0.0.1:8080", "return 301 https://$host$request_uri;"} {
			if !strings.Contains(conf, want) {
				t.Fatalf("下发配置应含 %q:\n%s", want, conf)
			}
		}
		// 临时目录 700 + 私钥收紧 + 证书落位 + conf.d 覆盖(先删陈旧备份)+ 校验 + 热加载 + 清理。
		for _, prefix := range [][]string{
			{"mkdir", "-p", tmpDir},
			{"chmod", "700", tmpDir},
			{"chmod", "600", tmpKeyPath},
			{"mkdir", "-p", certDir("pip.efg.com")},
			{"cp", tmpCertPath, certDir("pip.efg.com") + "/fullchain.pem"},
			{"cp", tmpKeyPath, certDir("pip.efg.com") + "/privkey.pem"},
			{"chmod", "600", certDir("pip.efg.com") + "/privkey.pem"},
			{"rm", "-f", backupConfFmt},
			{"cp", tmpConfPath, managedConfPath},
			{"nginx", "-t"},
			{"nginx", "-s", "reload"},
			{"rm", "-rf", tmpDir},
		} {
			if !hasCmd(ft, prefix...) {
				t.Fatalf("缺少命令 %v:%v", prefix, ft.execCalls)
			}
		}
		// 生效上游展示。
		if up := svc.EffectiveUpstream(s); up != "127.0.0.1:8080" {
			t.Fatalf("EffectiveUpstream 应 127.0.0.1:8080,得 %s", up)
		}
	})
}

func TestApplyCustomUpstream(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc, ft, _ := newTestService(t, st.DB)
		nginxOK(ft)
		ctx := context.Background()
		if _, err := svc.SaveSettings(ctx, SettingsInput{
			Enabled: boolPtr(true), ServerID: strPtr("srv-1"), Domain: strPtr("pip.efg.com"),
			CertID: strPtr("c1"), UpstreamHost: strPtr("10.0.0.8"), UpstreamPort: intPtr(3000),
		}); err != nil {
			t.Fatalf("保存:%v", err)
		}
		if _, err := svc.Apply(ctx); err != nil {
			t.Fatalf("应用:%v", err)
		}
		if conf := ft.uploadBytes[tmpConfPath]; !strings.Contains(conf, "proxy_pass http://10.0.0.8:3000;") {
			t.Fatalf("自定义上游应渲染:\n%s", conf)
		}
	})
}

func TestApplyNonRootSudo(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc, ft, _ := newTestService(t, st.DB)
		nginxOK(ft)
		// 非 root + 免密 sudo 可用。
		inner := ft.resultFor
		ft.resultFor = func(cmd []string) *target.ExecResult {
			if strings.Join(cmd, " ") == "id -u" {
				return &target.ExecResult{ExitCode: 0, Stdout: "1000"}
			}
			return inner(cmd)
		}
		ctx := context.Background()
		enableSettings(t, svc)
		if _, err := svc.Apply(ctx); err != nil {
			t.Fatalf("应用:%v", err)
		}
		if !hasCmd(ft, "sudo", "-n", "nginx", "-t") || !hasCmd(ft, "sudo", "-n", "cp", tmpConfPath, managedConfPath) {
			t.Fatalf("非 root 应经 sudo -n 执行:%v", ft.execCalls)
		}
	})
}

func TestApplyNoNginx(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc, ft, _ := newTestService(t, st.DB)
		ft.resultFor = func(cmd []string) *target.ExecResult {
			if strings.Join(cmd, " ") == "nginx -v" {
				return &target.ExecResult{ExitCode: 127, Stderr: "command not found"}
			}
			return nil
		}
		ctx := context.Background()
		enableSettings(t, svc)
		_, err := svc.Apply(ctx)
		if err == nil || !errors.Is(err, ErrNoNginx) {
			t.Fatalf("未装 nginx 应 ErrNoNginx,得 %v", err)
		}
		s, _ := svc.GetSettings(ctx)
		if s.Status != StatusFailed {
			t.Fatalf("失败应记入状态,得 %q", s.Status)
		}
	})
}

func TestApplyNoPrivilege(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc, ft, _ := newTestService(t, st.DB)
		ft.resultFor = func(cmd []string) *target.ExecResult {
			switch strings.Join(cmd, " ") {
			case "id -u":
				return &target.ExecResult{ExitCode: 0, Stdout: "1000"}
			case "sudo -n true":
				return &target.ExecResult{ExitCode: 1, Stderr: "sudo: a password is required"}
			}
			return nil
		}
		ctx := context.Background()
		enableSettings(t, svc)
		_, err := svc.Apply(ctx)
		if err == nil || !errors.Is(err, ErrNoPrivilege) {
			t.Fatalf("无提权应 ErrNoPrivilege,得 %v", err)
		}
	})
}

func TestApplyNginxTestFailRollback(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc, ft, _ := newTestService(t, st.DB)
		nginxOK(ft)
		inner := ft.resultFor
		ft.resultFor = func(cmd []string) *target.ExecResult {
			joined := strings.Join(cmd, " ")
			switch {
			case joined == "nginx -t":
				return &target.ExecResult{ExitCode: 1, Stderr: "nginx: [emerg] unexpected ;"}
			case strings.HasPrefix(joined, "cp "+managedConfPath+" "):
				return &target.ExecResult{ExitCode: 1, Stderr: "No such file"} // 首次应用无备份源
			case joined == "test -f "+backupConfFmt:
				return &target.ExecResult{ExitCode: 1} // 备份不存在(与上一条一致)
			}
			return inner(cmd)
		}
		ctx := context.Background()
		enableSettings(t, svc)
		_, err := svc.Apply(ctx)
		if err == nil || !errors.Is(err, ErrApply) {
			t.Fatalf("校验失败应 ErrApply,得 %v", err)
		}
		// 无备份 → 回滚摘除我们的 conf.d 文件。
		if !hasCmd(ft, "rm", "-f", managedConfPath) {
			t.Fatalf("回滚应摘除 conf.d 文件:%v", ft.execCalls)
		}
		if hasCmd(ft, "nginx", "-s", "reload") {
			t.Fatalf("校验失败不应 reload:%v", ft.execCalls)
		}
		s, _ := svc.GetSettings(ctx)
		if s.Status != StatusFailed || s.StatusDetail == "" {
			t.Fatalf("失败应记入状态/详情,得 %q/%q", s.Status, s.StatusDetail)
		}
	})
}

// --- 禁用 ---

func TestDisable(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc, ft, _ := newTestService(t, st.DB)
		nginxOK(ft)
		ctx := context.Background()
		enableSettings(t, svc)
		if _, err := svc.Apply(ctx); err != nil {
			t.Fatalf("应用:%v", err)
		}

		ft.execCalls = nil
		s, err := svc.Disable(ctx)
		if err != nil {
			t.Fatalf("禁用:%v", err)
		}
		if s.Enabled || s.Status != "" {
			t.Fatalf("禁用后应 enabled=false 且状态清空,得 %v/%q", s.Enabled, s.Status)
		}
		for _, prefix := range [][]string{
			{"rm", "-f", managedConfPath},
			{"rm", "-rf", certDir("pip.efg.com")},
			{"nginx", "-t"},
			{"nginx", "-s", "reload"},
		} {
			if !hasCmd(ft, prefix...) {
				t.Fatalf("缺少清理命令 %v:%v", prefix, ft.execCalls)
			}
		}

		// 再次禁用(远端已无平台配置)→ no-op,不触 nginx。
		ft.execCalls = nil
		if _, err := svc.Disable(ctx); err != nil {
			t.Fatalf("重复禁用:%v", err)
		}
		if hasCmd(ft, "nginx") {
			t.Fatalf("远端无平台配置时禁用不应触 nginx:%v", ft.execCalls)
		}
	})
}

// --- certmgmt 联动 ---

func TestUsesCertAndRedeploy(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc, ft, _ := newTestService(t, st.DB)
		nginxOK(ft)
		ctx := context.Background()
		enableSettings(t, svc)

		// 未应用(status 为空):远端无配置,不占用、不联动。
		if used, _ := svc.UsesCert(ctx, "c1"); used {
			t.Fatalf("未应用时 c1 不应被占用")
		}
		ft.execCalls = nil
		if err := svc.RedeployCert(ctx, "c1"); err != nil {
			t.Fatalf("未应用时联动应为 no-op:%v", err)
		}
		if len(ft.execCalls) != 0 {
			t.Fatalf("no-op 不应有命令:%v", ft.execCalls)
		}

		// 应用后:占用按 status 非空判定(与 enabled 无关)。
		if _, err := svc.Apply(ctx); err != nil {
			t.Fatalf("应用:%v", err)
		}
		if used, _ := svc.UsesCert(ctx, "c1"); !used {
			t.Fatalf("c1 应被占用")
		}
		if used, _ := svc.UsesCert(ctx, "c2"); used {
			t.Fatalf("c2 不应被占用")
		}
		// 仅关掉启用开关(未禁用清理):远端仍在跑,占用必须保持。
		if _, err := svc.SaveSettings(ctx, SettingsInput{Enabled: boolPtr(false)}); err != nil {
			t.Fatalf("关闭启用:%v", err)
		}
		if used, _ := svc.UsesCert(ctx, "c1"); !used {
			t.Fatalf("仅关开关未清理时 c1 仍应被占用")
		}
		if _, err := svc.SaveSettings(ctx, SettingsInput{Enabled: boolPtr(true)}); err != nil {
			t.Fatalf("重新启用:%v", err)
		}

		// 未引用的证书 → no-op(不触发任何远端命令)。
		ft.execCalls = nil
		if err := svc.RedeployCert(ctx, "c2"); err != nil {
			t.Fatalf("未引用证书的联动应为 no-op:%v", err)
		}
		if len(ft.execCalls) != 0 {
			t.Fatalf("no-op 不应有命令:%v", ft.execCalls)
		}
		// 引用中的证书 → 重新应用。
		if err := svc.RedeployCert(ctx, "c1"); err != nil {
			t.Fatalf("联动重下发:%v", err)
		}
		if !hasCmd(ft, "nginx", "-s", "reload") {
			t.Fatalf("联动应重新应用:%v", ft.execCalls)
		}
		s, _ := svc.GetSettings(ctx)
		if s.Status != StatusActive {
			t.Fatalf("联动后应 active,得 %q", s.Status)
		}

		// 禁用清理后:不再占用。
		if _, err := svc.Disable(ctx); err != nil {
			t.Fatalf("禁用:%v", err)
		}
		if used, _ := svc.UsesCert(ctx, "c1"); used {
			t.Fatalf("禁用后 c1 不应被占用")
		}
	})
}

// TestSaveSettingsAppliedChange:已应用(status 非空)时变更服务器/域名被拒(防旧机/旧域名
// 配置与私钥成孤儿);换证书允许但 status 重置为待应用;禁用清理后可自由变更。
func TestSaveSettingsAppliedChange(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc, ft, _ := newTestService(t, st.DB)
		nginxOK(ft)
		ctx := context.Background()
		enableSettings(t, svc)
		if _, err := svc.Apply(ctx); err != nil {
			t.Fatalf("应用:%v", err)
		}

		// 变更服务器 → 拒绝。
		_, err := svc.SaveSettings(ctx, SettingsInput{ServerID: strPtr("srv-2")})
		if err == nil || !errors.Is(err, ErrAppliedChange) {
			t.Fatalf("已应用时换服务器应 ErrAppliedChange,得 %v", err)
		}
		// 变更域名 → 拒绝。
		_, err = svc.SaveSettings(ctx, SettingsInput{Domain: strPtr("www.efg.com")})
		if err == nil || !errors.Is(err, ErrAppliedChange) {
			t.Fatalf("已应用时换域名应 ErrAppliedChange,得 %v", err)
		}

		// 换证书(c4 同域覆盖)→ 允许,status 重置为待应用。
		s, err := svc.SaveSettings(ctx, SettingsInput{CertID: strPtr("c4")})
		if err != nil {
			t.Fatalf("换证书应允许:%v", err)
		}
		if s.CertID != "c4" || s.Status != "" {
			t.Fatalf("换证书后应重置状态,得 %s/%q", s.CertID, s.Status)
		}
		// 重新应用 → 收敛到新证书,占用随 status 恢复。
		if _, err := svc.Apply(ctx); err != nil {
			t.Fatalf("重新应用:%v", err)
		}
		if used, _ := svc.UsesCert(ctx, "c4"); !used {
			t.Fatalf("重新应用后 c4 应被占用")
		}

		// 禁用清理后 → 可自由变更服务器。
		if _, err := svc.Disable(ctx); err != nil {
			t.Fatalf("禁用:%v", err)
		}
		if _, err := svc.SaveSettings(ctx, SettingsInput{ServerID: strPtr("srv-2")}); err != nil {
			t.Fatalf("清理后换服务器应允许:%v", err)
		}
	})
}

// --- 探测 ---

func TestDetect(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc, ft, _ := newTestService(t, st.DB)
		ft.resultFor = func(cmd []string) *target.ExecResult {
			joined := strings.Join(cmd, " ")
			switch {
			case joined == "nginx -v":
				return &target.ExecResult{ExitCode: 0, Stderr: "nginx version: nginx/1.26.2"}
			case joined == "nginx -T":
				return &target.ExecResult{ExitCode: 0, Stdout: "include /etc/nginx/conf.d/*.conf;\n..."}
			case joined == "id -u":
				return &target.ExecResult{ExitCode: 0, Stdout: "0"}
			case joined == "test -f "+managedConfPath:
				return &target.ExecResult{ExitCode: 0}
			}
			return nil
		}
		d, err := svc.Detect(context.Background(), "srv-1")
		if err != nil {
			t.Fatalf("探测:%v", err)
		}
		if !d.Installed || d.Version != "1.26.2" || !d.IsRoot || !d.SudoOk || !d.ConfDIncluded || !d.ManagedConf {
			t.Fatalf("探测结果不符:%+v", d)
		}

		// 未安装 nginx:Installed=false 且 sudo 探测照常。
		ft2 := &fakeTarget{}
		svc2 := New(st.DB, ft2, newFakeCerts(), 8080)
		ft2.resultFor = func(cmd []string) *target.ExecResult {
			if strings.Join(cmd, " ") == "nginx -v" {
				return &target.ExecResult{ExitCode: 127, Stderr: "command not found"}
			}
			if strings.Join(cmd, " ") == "id -u" {
				return &target.ExecResult{ExitCode: 0, Stdout: "0"}
			}
			return nil
		}
		d2, err := svc2.Detect(context.Background(), "srv-1")
		if err != nil {
			t.Fatalf("探测:%v", err)
		}
		if d2.Installed || !d2.IsRoot {
			t.Fatalf("未安装 nginx 应 Installed=false:%+v", d2)
		}
	})
}

// TestDetectSudoPrefix:非 root + 免密 sudo 的机器,探测的 nginx -T 必须带 sudo -n 前缀
// (裸跑读不到 root 600 的证书/私钥,会误报 conf.d 未加载)。
func TestDetectSudoPrefix(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc, ft, _ := newTestService(t, st.DB)
		ft.resultFor = func(cmd []string) *target.ExecResult {
			joined := strings.Join(cmd, " ")
			switch {
			case joined == "id -u":
				return &target.ExecResult{ExitCode: 0, Stdout: "1000"}
			case joined == "sudo -n true":
				return &target.ExecResult{ExitCode: 0}
			case joined == "nginx -v":
				return &target.ExecResult{ExitCode: 0, Stderr: "nginx version: nginx/1.26.2"}
			case joined == "sudo -n nginx -T":
				return &target.ExecResult{ExitCode: 0, Stdout: "include /etc/nginx/conf.d/*.conf;"}
			case joined == "nginx -T":
				return &target.ExecResult{ExitCode: 1, Stderr: "open() failed (13: Permission denied)"}
			}
			return nil
		}
		d, err := svc.Detect(context.Background(), "srv-1")
		if err != nil {
			t.Fatalf("探测:%v", err)
		}
		if !d.SudoOk || d.IsRoot {
			t.Fatalf("应识别非 root + sudo 可用:%+v", d)
		}
		if !d.ConfDIncluded {
			t.Fatalf("带提权的 nginx -T 应识别 conf.d 已加载:%+v", d)
		}
		if !hasCmd(ft, "sudo", "-n", "nginx", "-T") {
			t.Fatalf("nginx -T 应带 sudo -n 前缀:%v", ft.execCalls)
		}
	})
}
