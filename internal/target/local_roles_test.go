package target

// local_roles_test.go 覆盖 WithLocalHostRoles 的角色开关语义(specs/local-node-roles.md 4.2/4.3):
//   - 禁用:__local__ 的执行类请求显式 ErrLocalRoleDisabled,绝不静默落到本机执行;
//   - 启用:语义与旧 WithLocalHost 一致(合成视图带池参数、本地执行);
//   - List 扩展仅 InList && Enabled 时追加;旧 WithLocalHost 的 List 逐字透传(冻结);
//   - 真实 serverID 一切方法逐字透传(关闭态逐字节不变的红线)。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func rolesFn(enabled, inList bool) RolesFn {
	return func() LocalRoles {
		return LocalRoles{Enabled: enabled, InList: inList, Labels: "linux,arch=arm64", MaxBuilds: 3, Priority: 7}
	}
}

// TestLocalRolesDisabledGates:禁用角色对 __local__ 的执行类请求一律 ErrLocalRoleDisabled。
func TestLocalRolesDisabledGates(t *testing.T) {
	svc := WithLocalHostRoles(&fakeBaseService{}, rolesFn(false, false))
	ctx := context.Background()

	if _, err := svc.Get(ctx, LocalHostID); !errors.Is(err, ErrLocalRoleDisabled) {
		t.Fatalf("Get 应 ErrLocalRoleDisabled,得 %v", err)
	}
	if _, err := svc.Test(ctx, LocalHostID); !errors.Is(err, ErrLocalRoleDisabled) {
		t.Fatalf("Test 应 ErrLocalRoleDisabled,得 %v", err)
	}
	if _, err := svc.Exec(ctx, LocalHostID, []string{"echo", "hi"}); !errors.Is(err, ErrLocalRoleDisabled) {
		t.Fatalf("Exec 应 ErrLocalRoleDisabled,得 %v", err)
	}
	if _, err := svc.ExecWithStdin(ctx, LocalHostID, []string{"cat"}, strings.NewReader("x")); !errors.Is(err, ErrLocalRoleDisabled) {
		t.Fatalf("ExecWithStdin 应 ErrLocalRoleDisabled,得 %v", err)
	}
	if _, err := svc.ExecStream(ctx, LocalHostID, []string{"tail"}); !errors.Is(err, ErrLocalRoleDisabled) {
		t.Fatalf("ExecStream 应 ErrLocalRoleDisabled,得 %v", err)
	}
	if err := svc.Upload(ctx, LocalHostID, strings.NewReader("x"), "/tmp/x"); !errors.Is(err, ErrLocalRoleDisabled) {
		t.Fatalf("Upload 应 ErrLocalRoleDisabled,得 %v", err)
	}
	if _, err := svc.Download(ctx, LocalHostID, "/tmp/x", &strings.Builder{}); !errors.Is(err, ErrLocalRoleDisabled) {
		t.Fatalf("Download 应 ErrLocalRoleDisabled,得 %v", err)
	}
	if err := svc.DockerLogin(ctx, LocalHostID, "", "cred-1"); !errors.Is(err, ErrLocalRoleDisabled) {
		t.Fatalf("DockerLogin 应 ErrLocalRoleDisabled,得 %v", err)
	}

	// 不变量与开关无关:仍不可作为登记服务器改写/删除。
	if _, err := svc.Update(ctx, LocalHostID, UpdateInput{}); !errors.Is(err, ErrLocalHostImmutable) {
		t.Fatalf("禁用态 Update 应仍 ErrLocalHostImmutable,得 %v", err)
	}
	if err := svc.Delete(ctx, LocalHostID); !errors.Is(err, ErrLocalHostImmutable) {
		t.Fatalf("禁用态 Delete 应仍 ErrLocalHostImmutable,得 %v", err)
	}

	// 真实 serverID 逐字透传(关闭态零行为变化的红线)。
	if got, err := svc.Get(ctx, "real-1"); err != nil || got.Name != "web-1" {
		t.Fatalf("透传 Get = %+v, %v", got, err)
	}
	if res, err := svc.Exec(ctx, "real-1", []string{"uname"}); err != nil || res.Stdout != "ssh-passthrough" {
		t.Fatalf("透传 Exec = %+v, %v", res, err)
	}
}

// TestLocalRolesEnabled:启用后语义与旧包装一致,合成视图携带池参数。
func TestLocalRolesEnabled(t *testing.T) {
	svc := WithLocalHostRoles(&fakeBaseService{}, rolesFn(true, false))
	ctx := context.Background()

	srv, err := svc.Get(ctx, LocalHostID)
	if err != nil {
		t.Fatal(err)
	}
	if srv.ID != LocalHostID || srv.Labels != "linux,arch=arm64" || srv.MaxBuilds != 3 || srv.Priority != 7 {
		t.Fatalf("合成视图应带池参数:%+v", srv)
	}
	if srv.CredentialID != "" {
		t.Fatalf("合成视图绝无凭据引用:%+v", srv)
	}
	if res, err := svc.Test(ctx, LocalHostID); err != nil || !res.OK {
		t.Fatalf("Test 应恒可达:%+v, %v", res, err)
	}
	// Windows 无独立 echo 可执行文件(echo 是 cmd 内建命令),按平台选用可执行程序,
	// 保证本机执行路径在 CI 的 Windows 宿主上同样被覆盖(而非 skip)。
	echoCmd := []string{"echo", "local-ok"}
	if runtime.GOOS == "windows" {
		echoCmd = []string{"cmd", "/c", "echo", "local-ok"}
	}
	if res, err := svc.Exec(ctx, LocalHostID, echoCmd); err != nil || res.ExitCode != 0 {
		t.Fatalf("本机执行:%+v, %v", res, err)
	}
}

// TestLocalRolesListExtension:List 仅在 Enabled && InList 时追加本机;旧包装冻结透传。
func TestLocalRolesListExtension(t *testing.T) {
	ctx := context.Background()
	base := &fakeListerService{}

	// InList=false(网关/调度器包装):不追加。
	noList := WithLocalHostRoles(base, rolesFn(true, false))
	if got, err := noList.List(ctx); err != nil || len(got) != 1 {
		t.Fatalf("InList=false 不应追加:%d 项, %v", len(got), err)
	}
	// Enabled=false(deploy 关):不追加(零命中语义不变)。
	offList := WithLocalHostRoles(base, rolesFn(false, true))
	if got, err := offList.List(ctx); err != nil || len(got) != 1 {
		t.Fatalf("禁用态不应追加:%d 项, %v", len(got), err)
	}
	// Enabled && InList(deploy 开):尾部追加合成 Server,携带 labels。
	onList := WithLocalHostRoles(base, rolesFn(true, true))
	got, err := onList.List(ctx)
	if err != nil || len(got) != 2 {
		t.Fatalf("开启态应追加本机:%d 项, %v", len(got), err)
	}
	if got[1].ID != LocalHostID || got[1].Labels != "linux,arch=arm64" || got[1].Kind != "" || got[1].IsPVENode() {
		t.Fatalf("追加项应为带 labels 的本机合成视图(Kind 空 → 非pve):%+v", got[1])
	}
	// 旧 WithLocalHost(roles=nil):List 冻结逐字透传。
	legacy := WithLocalHost(base)
	legacyGot, err := legacy.List(ctx)
	if err != nil || len(legacyGot) != 1 {
		t.Fatalf("旧包装 List 冻结透传:%d 项, %v", len(legacyGot), err)
	}
}

// TestLocalRolesDownloadLocal:Download 本地实现按路径流式读回,不存在 → 透传 os 错误。
func TestLocalRolesDownloadLocal(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "artifact.bin")
	if err := os.WriteFile(p, []byte("hello-download"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := WithLocalHostRoles(&fakeBaseService{}, rolesFn(true, false))
	var sb strings.Builder
	n, err := svc.Download(context.Background(), LocalHostID, p, &sb)
	if err != nil || n != int64(len("hello-download")) || sb.String() != "hello-download" {
		t.Fatalf("本地 Download:n=%d err=%v data=%q", n, err, sb.String())
	}
	if _, err := svc.Download(context.Background(), LocalHostID, filepath.Join(dir, "missing"), &sb); err == nil {
		t.Fatal("文件不存在应报错")
	}
}

// TestLocalDockerLoginGuards:DockerLogin 的凭据契约守卫(执行路径由真机手工验证,
// 单测不打真实 docker)。
func TestLocalDockerLoginGuards(t *testing.T) {
	ctx := context.Background()
	svc := WithLocalHostRoles(&fakeBaseService{}, rolesFn(true, false))

	// 凭据引用为空 → 明确错误。
	if err := svc.DockerLogin(ctx, LocalHostID, "", ""); err == nil || strings.Contains(err.Error(), "127") {
		t.Fatalf("空凭据应被守卫拦截,得 %v", err)
	}
	// 基类未实现 RegistryCredentialProvider → 明确错误(不静默跳过)。
	err := svc.DockerLogin(ctx, LocalHostID, "registry.local", "cred-1")
	if err == nil || !strings.Contains(err.Error(), "凭据提供者未装配") {
		t.Fatalf("缺凭据提供者应报人读错误,得 %v", err)
	}
}

// fakeListerService 提供 List 的最小 fake(其余方法嵌入接口 nil panic 即未触达)。
type fakeListerService struct {
	Service
}

func (f *fakeListerService) List(context.Context) ([]*Server, error) {
	return []*Server{{ID: "real-1", Name: "web-1"}}, nil
}

// fakeCredProvider 实现 RegistryCredentialProvider(供装配层断言类型断言路径;执行路径
// 不在单测触达)。当前用例只验证「未实现」分支,fake 保留给后续本机 DockerLogin 集成用例。
type fakeCredProvider struct {
	fakeBaseService
}

func (f fakeCredProvider) OpenRegistryCredential(_ context.Context, _ string) (string, string, error) {
	return "u", "p", nil
}
