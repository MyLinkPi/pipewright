package deploy

import (
	"context"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/target"
)

// cleanupTargetImages:只删同仓库的旧 tag;新 ref、回滚目标(digest 匹配)、dangling、
// 其它仓库的镜像绝不碰。
func TestCleanupTargetImages(t *testing.T) {
	st := &stubTarget{servers: map[string]*target.Server{"s1": {ID: "s1"}}}
	// 目标机上的镜像清单:新 ref(v3)、回滚目标(v2,ID=prev 短前缀)、旧 v1、dangling、其它仓库。
	imagesOut := strings.Join([]string{
		"reg.example.com/org/app:v3\tidv3aaa",
		"reg.example.com/org/app:v2\tidv2bbb",
		"reg.example.com/org/app:v1\tidv1ccc",
		"reg.example.com/org/app:v0\tidv0ddd",
		"<none>:<none>\tidnone0",
		"other.example.com/app:v9\tidoth00",
	}, "\n")
	st.execFn = func(_ string, cmd []string) (*target.ExecResult, error) {
		if len(cmd) >= 2 && cmd[1] == "images" {
			return &target.ExecResult{Stdout: imagesOut, ExitCode: 0}, nil
		}
		return &target.ExecResult{ExitCode: 0}, nil
	}
	s := &service{targets: st}

	// 新 ref=v3;回滚 digest 的短 ID = idv2bbb → v2 保留;v1/v0 删。
	s.cleanupTargetImages(context.Background(), "s1", "reg.example.com/org/app:v3", "sha256:idv2bbb0000000000000000000000000000000000000000000000000000ff")

	var rmi []string
	for _, c := range st.calls {
		if len(c) == 3 && c[0] == "docker" && c[1] == "rmi" {
			rmi = append(rmi, c[2])
		}
	}
	if len(rmi) != 2 || rmi[0] != "reg.example.com/org/app:v1" && rmi[0] != "reg.example.com/org/app:v0" {
		// v1 与 v0 都该删(顺序随清单);严格断言集合。
	}
	got := map[string]bool{}
	for _, r := range rmi {
		got[r] = true
	}
	if len(rmi) != 2 || !got["reg.example.com/org/app:v1"] || !got["reg.example.com/org/app:v0"] {
		t.Fatalf("应只删 v1/v0,实际 rmi:%v", rmi)
	}
	for _, keep := range []string{
		"reg.example.com/org/app:v3",
		"reg.example.com/org/app:v2",
		"other.example.com/app:v9",
	} {
		if got[keep] {
			t.Fatalf("不应删除 %s", keep)
		}
	}
}

// 无回滚目标(首次部署)→ 只保留新 ref,其余同仓库旧 tag 全删。
func TestCleanupTargetImagesFirstDeploy(t *testing.T) {
	st := &stubTarget{servers: map[string]*target.Server{"s1": {ID: "s1"}}}
	imagesOut := "reg.example.com/org/app:v2\tidv2bbb\nreg.example.com/org/app:v1\tidv1ccc\n"
	st.execFn = func(_ string, cmd []string) (*target.ExecResult, error) {
		if len(cmd) >= 2 && cmd[1] == "images" {
			return &target.ExecResult{Stdout: imagesOut, ExitCode: 0}, nil
		}
		return &target.ExecResult{ExitCode: 0}, nil
	}
	s := &service{targets: st}
	s.cleanupTargetImages(context.Background(), "s1", "reg.example.com/org/app:v2", "")

	var rmi []string
	for _, c := range st.calls {
		if len(c) == 3 && c[1] == "rmi" {
			rmi = append(rmi, c[2])
		}
	}
	if len(rmi) != 1 || rmi[0] != "reg.example.com/org/app:v1" {
		t.Fatalf("首次部署应只删 v1,实际:%v", rmi)
	}
}

// 列镜像失败 → 静默放弃,不执行任何 rmi(best-effort 不影响部署)。
func TestCleanupTargetImagesSilentOnListFailure(t *testing.T) {
	st := &stubTarget{servers: map[string]*target.Server{"s1": {ID: "s1"}}}
	st.execFn = func(_ string, _ []string) (*target.ExecResult, error) {
		return &target.ExecResult{ExitCode: 1, Stderr: "boom"}, nil
	}
	s := &service{targets: st}
	s.cleanupTargetImages(context.Background(), "s1", "reg.example.com/org/app:v2", "")
	for _, c := range st.calls {
		if len(c) >= 2 && c[1] == "rmi" {
			t.Fatalf("列镜像失败时不应 rmi:%v", c)
		}
	}
}

// imageRepoOfRef:registry 含端口时最后的 ':' 才是 tag 分隔;无 tag 原样。
func TestImageRepoOfRef(t *testing.T) {
	cases := map[string]string{
		"reg.example.com/org/app:v3": "reg.example.com/org/app",
		"localhost:5000/org/app:v1":  "localhost:5000/org/app",
		"reg.example.com/org/app":    "reg.example.com/org/app",
		"":                           "",
	}
	for in, want := range cases {
		if got := imageRepoOfRef(in); got != want {
			t.Fatalf("imageRepoOfRef(%q) = %q, want %q", in, got, want)
		}
	}
}
