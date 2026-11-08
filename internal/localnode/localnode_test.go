package localnode

import (
	"context"
	"errors"
	"runtime"
	"testing"

	"github.com/huangchengsir/pipewright/internal/store"
	"github.com/huangchengsir/pipewright/internal/storetest"
)

// TestSaveGetRoundTrip:单行 CRUD 往返;指针字段 nil = 不修改;默认态全关。
func TestSaveGetRoundTrip(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc := New(st.DB)
		ctx := context.Background()

		// 无行 = 全关(默认态)。
		r, err := svc.Get(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if r.BuildEnabled || r.DeployEnabled || r.GatewayEnabled || r.BuildLabels != "" {
			t.Fatalf("默认态应全关,得 %+v", r)
		}

		// 保存:开 build + labels/槽位/优先级。
		yes := true
		got, err := svc.Save(ctx, SaveInput{
			BuildEnabled: &yes, BuildLabels: strPtr("linux,arch=arm64"),
			BuildMaxBuilds: intPtr(3), BuildPriority: intPtr(7),
		})
		if err != nil {
			t.Fatal(err)
		}
		if !got.BuildEnabled || got.BuildLabels != "linux,arch=arm64" || got.BuildMaxBuilds != 3 || got.BuildPriority != 7 {
			t.Fatalf("保存往返不符:%+v", got)
		}
		r2, err := svc.Get(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if r2 != got {
			t.Fatalf("Get 回读不一致:%+v vs %+v", r2, got)
		}

		// 部分更新(nil = 不修改):只改 labels,其余保持。
		got2, err := svc.Save(ctx, SaveInput{BuildLabels: strPtr("gpu")})
		if err != nil {
			t.Fatal(err)
		}
		if !got2.BuildEnabled || got2.BuildLabels != "gpu" || got2.BuildMaxBuilds != 3 || got2.BuildPriority != 7 {
			t.Fatalf("部分更新丢失原值:%+v", got2)
		}
	})
}

func strPtr(s string) *string { return &s }
func intPtr(i int) *int       { return &i }

// TestValidation:labels 非法 / 槽位越界 / 优先级越界 → ErrInvalidInput。
func TestValidation(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc := New(st.DB)
		ctx := context.Background()

		for _, tc := range []struct {
			name string
			in   SaveInput
		}{
			{"labels 非法字符", SaveInput{BuildLabels: strPtr("lin ux")}},
			{"labels 下划线开头", SaveInput{BuildLabels: strPtr("_bad")}},
			{"槽位越界", SaveInput{BuildMaxBuilds: intPtr(65)}},
			{"槽位负数", SaveInput{BuildMaxBuilds: intPtr(-1)}},
			{"优先级越界", SaveInput{BuildPriority: intPtr(101)}},
			{"优先级负数", SaveInput{BuildPriority: intPtr(-1)}},
		} {
			if _, err := svc.Save(ctx, tc.in); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("%s:应 ErrInvalidInput,得 %v", tc.name, err)
			}
		}
	})
}

// TestPlatformGate:非 Linux 控制机开 deploy/gateway → ErrPlatformUnsupported;
// Linux 下该用例退化为开通成功;build 不受限(任何平台)。
func TestPlatformGate(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc := New(st.DB)
		ctx := context.Background()
		yes := true

		// build 不受 OS 门禁。
		if _, err := svc.Save(ctx, SaveInput{BuildEnabled: &yes}); err != nil {
			t.Fatalf("build 角色不应受 OS 门禁限制:%v", err)
		}

		_, deployErr := svc.Save(ctx, SaveInput{DeployEnabled: &yes})
		_, gatewayErr := svc.Save(ctx, SaveInput{GatewayEnabled: &yes})
		if runtime.GOOS == "linux" {
			if deployErr != nil || gatewayErr != nil {
				t.Fatalf("Linux 控制机应可开 deploy/gateway:%v / %v", deployErr, gatewayErr)
			}
			return
		}
		if !errors.Is(deployErr, ErrPlatformUnsupported) || !errors.Is(gatewayErr, ErrPlatformUnsupported) {
			t.Fatalf("非 Linux 控制机开 deploy/gateway 应 ErrPlatformUnsupported:%v / %v", deployErr, gatewayErr)
		}
	})
}
