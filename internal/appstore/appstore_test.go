package appstore

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/store"
	"github.com/huangchengsir/pipewright/internal/storetest"
)

func newTestService(t *testing.T, db *store.Store) Service {
	t.Helper()
	svc := New(db.DB)
	if err := svc.EnsureBuiltins(context.Background()); err != nil {
		t.Fatalf("seed builtins: %v", err)
	}
	return svc
}

func TestEnsureBuiltinsIdempotent(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc := newTestService(t, st)
		// 二次 seed 不报错、不翻倍。
		if err := svc.EnsureBuiltins(context.Background()); err != nil {
			t.Fatalf("二次 seed:%v", err)
		}
		list, err := svc.List(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != len(builtins) {
			t.Fatalf("内置模板数不符:%d != %d", len(list), len(builtins))
		}
		for _, tpl := range list {
			if !tpl.Builtin {
				t.Fatalf("seed 出来的应全为内置:%+v", tpl)
			}
		}
		// 内置模板不可删/改。
		if err := svc.Delete(context.Background(), list[0].ID); !errors.Is(err, ErrBuiltin) {
			t.Fatalf("删内置应 ErrBuiltin,得 %v", err)
		}
		desc := "x"
		if _, err := svc.Update(context.Background(), list[0].ID, UpdateInput{Description: &desc}); !errors.Is(err, ErrBuiltin) {
			t.Fatalf("改内置应 ErrBuiltin,得 %v", err)
		}
	})
}

func TestRenderParams(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc := newTestService(t, st)
		ctx := context.Background()
		list, _ := svc.List(ctx)
		var mysql *Template
		for i := range list {
			if list[i].Name == "mysql" {
				mysql = &list[i]
			}
		}
		if mysql == nil {
			t.Fatal("缺 mysql 内置模板")
		}

		// 1) 全默认 + secret 自动生成。
		res, err := svc.Render(ctx, mysql.ID, map[string]string{})
		if err != nil {
			t.Fatalf("渲染:%v", err)
		}
		pw := res.Used["root_password"]
		if len(pw) < 16 || strings.Contains(res.Compose, "{{") {
			t.Fatalf("secret 应自动生成且占位符应全替换:%q\n%s", pw, res.Compose)
		}
		if !strings.Contains(res.Compose, "mysql:8.4") || !strings.Contains(res.Compose, "3306:3306") {
			t.Fatalf("默认值未生效:\n%s", res.Compose)
		}
		if !strings.Contains(res.Compose, pw) {
			t.Fatalf("secret 应替换进 compose:\n%s", res.Compose)
		}

		// 2) 显式参数覆盖 + int 校验。
		res, err = svc.Render(ctx, mysql.ID, map[string]string{"version": "8.0", "port": "13306", "root_password": "pw123456"})
		if err != nil || !strings.Contains(res.Compose, "mysql:8.0") || !strings.Contains(res.Compose, "13306:3306") {
			t.Fatalf("显式参数未生效:%v\n%s", err, res.Compose)
		}
		if _, err := svc.Render(ctx, mysql.ID, map[string]string{"port": "70000"}); !errors.Is(err, ErrInvalidValue) {
			t.Fatalf("端口越界应 ErrInvalidValue,得 %v", err)
		}
		if _, err := svc.Render(ctx, mysql.ID, map[string]string{"port": "abc"}); !errors.Is(err, ErrInvalidValue) {
			t.Fatalf("非数字端口应 ErrInvalidValue,得 %v", err)
		}
		// 3) 未知参数拒绝;YAML 注入拒绝。
		if _, err := svc.Render(ctx, mysql.ID, map[string]string{"typo_param": "1"}); !errors.Is(err, ErrUnknownParam) {
			t.Fatalf("未知参数应 ErrUnknownParam,得 %v", err)
		}
		if _, err := svc.Render(ctx, mysql.ID, map[string]string{"root_password": "a\nb: evil"}); !errors.Is(err, ErrInvalidValue) {
			t.Fatalf("换行注入应 ErrInvalidValue,得 %v", err)
		}

		// 4) gitea:SSH_DOMAIN 必填、SSH_PORT 跟宿主映射走、SSH_LISTEN_PORT 恒 22;port 即 Web 端口。
		var gitea *Template
		for i := range list {
			if list[i].Name == "gitea" {
				gitea = &list[i]
			}
		}
		if gitea == nil {
			t.Fatal("缺 gitea 内置模板")
		}
		if _, err := svc.Render(ctx, gitea.ID, map[string]string{}); !errors.Is(err, ErrMissingParam) {
			t.Fatalf("缺 ssh_domain 应 ErrMissingParam,得 %v", err)
		}
		res, err = svc.Render(ctx, gitea.ID, map[string]string{"ssh_domain": "git.example.com"})
		if err != nil || !strings.Contains(res.Compose, "gitea/gitea:latest") {
			t.Fatalf("gitea 默认渲染:%v\n%s", err, res.Compose)
		}
		if !strings.Contains(res.Compose, "3000:3000") || !strings.Contains(res.Compose, "2222:22") ||
			!strings.Contains(res.Compose, `SSH_DOMAIN: "git.example.com"`) ||
			!strings.Contains(res.Compose, `SSH_PORT: "2222"`) || !strings.Contains(res.Compose, `SSH_LISTEN_PORT: "22"`) {
			t.Fatalf("gitea 端口/域名渲染不符:\n%s", res.Compose)
		}
		res, err = svc.Render(ctx, gitea.ID, map[string]string{"ssh_domain": "10.0.0.8", "port": "13000", "ssh_port": "2222"})
		if err != nil || !strings.Contains(res.Compose, "13000:3000") || !strings.Contains(res.Compose, `SSH_DOMAIN: "10.0.0.8"`) {
			t.Fatalf("gitea 显式参数未生效:%v\n%s", err, res.Compose)
		}

		// 5) openvas:密码 secret 自动生成;Web 端口默认 9392;用户名默认 admin。
		var openvas *Template
		for i := range list {
			if list[i].Name == "openvas" {
				openvas = &list[i]
			}
		}
		if openvas == nil {
			t.Fatal("缺 openvas 内置模板")
		}
		res, err = svc.Render(ctx, openvas.ID, map[string]string{})
		if err != nil {
			t.Fatalf("openvas 默认渲染:%v", err)
		}
		pw = res.Used["password"]
		if len(pw) < 16 || !strings.Contains(res.Compose, pw) {
			t.Fatalf("openvas 密码应自动生成并替换:%q\n%s", pw, res.Compose)
		}
		if !strings.Contains(res.Compose, "9392:9392") || !strings.Contains(res.Compose, "USERNAME: admin") ||
			!strings.Contains(res.Compose, "openvas-data:/var/lib/openvas") {
			t.Fatalf("openvas 默认值未生效:\n%s", res.Compose)
		}

		// 6) trivy-server:令牌 secret 自动生成;--token 恒在 command 里;端口 4954。
		var trivySrv *Template
		for i := range list {
			if list[i].Name == "trivy-server" {
				trivySrv = &list[i]
			}
		}
		if trivySrv == nil {
			t.Fatal("缺 trivy-server 内置模板")
		}
		res, err = svc.Render(ctx, trivySrv.ID, map[string]string{})
		if err != nil {
			t.Fatalf("trivy-server 默认渲染:%v", err)
		}
		tok := res.Used["token"]
		if len(tok) < 16 || !strings.Contains(res.Compose, tok) || strings.Contains(res.Compose, "{{") {
			t.Fatalf("trivy-server 令牌应自动生成并替换:%q\n%s", tok, res.Compose)
		}
		if !strings.Contains(res.Compose, "4954:4954") || !strings.Contains(res.Compose, "--token") ||
			!strings.Contains(res.Compose, "trivy-cache:/root/.cache/trivy") {
			t.Fatalf("trivy-server 默认渲染不符:\n%s", res.Compose)
		}
		if !strings.Contains(res.Compose, `TRIVY_DB_REPOSITORY: "ghcr.io/aquasecurity/trivy-db"`) ||
			!strings.Contains(res.Compose, `HTTPS_PROXY: ""`) {
			t.Fatalf("trivy-server 镜像仓库/代理默认值不符:\n%s", res.Compose)
		}
	})
}

func TestCustomTemplateCRUD(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc := newTestService(t, st)
		ctx := context.Background()
		in := CreateInput{
			Name:        "my-app",
			DisplayName: "我的应用",
			ComposeYAML: "services:\n  web:\n    image: nginx:{{version}}\n",
			Params:      []ParamSpec{{Name: "version", Type: ParamTypeString, Default: "1"}},
		}
		tpl, err := svc.Create(ctx, in)
		if err != nil {
			t.Fatalf("创建:%v", err)
		}
		if _, err := svc.Create(ctx, in); !errors.Is(err, ErrNameTaken) {
			t.Fatalf("重名应 ErrNameTaken,得 %v", err)
		}
		bad := CreateInput{Name: "-nope", ComposeYAML: "x: 1"}
		if _, err := svc.Create(ctx, bad); !errors.Is(err, ErrInvalidName) {
			t.Fatalf("非法名应 ErrInvalidName,得 %v", err)
		}
		bad2 := CreateInput{Name: "ok-name", ComposeYAML: "services:\n  a:\n    image: x:{{bad name}}\n", Params: []ParamSpec{{Name: "bad name"}}}
		if _, err := svc.Create(ctx, bad2); !errors.Is(err, ErrInvalidParam) {
			t.Fatalf("非法参数名应 ErrInvalidParam,得 %v", err)
		}

		// 更新 + 渲染联动。
		newCompose := "services:\n  web:\n    image: nginx:{{v2}}\n"
		newParams := []ParamSpec{{Name: "v2", Type: ParamTypeString, Default: "2"}}
		updated, err := svc.Update(ctx, tpl.ID, UpdateInput{ComposeYAML: &newCompose, Params: newParams})
		if err != nil || updated.Params[0].Name != "v2" {
			t.Fatalf("更新:%v %+v", err, updated)
		}
		res, err := svc.Render(ctx, tpl.ID, map[string]string{})
		if err != nil || !strings.Contains(res.Compose, "nginx:2") {
			t.Fatalf("更新后渲染:%v\n%s", err, res.Compose)
		}
		if err := svc.Delete(ctx, tpl.ID); err != nil {
			t.Fatalf("删除:%v", err)
		}
		if _, err := svc.Render(ctx, tpl.ID, nil); !errors.Is(err, ErrNotFound) {
			t.Fatalf("删后渲染应 ErrNotFound,得 %v", err)
		}
	})
}

func TestMissingRequiredParam(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc := New(st.DB)
		ctx := context.Background()
		tpl, err := svc.Create(ctx, CreateInput{
			Name:        "strict",
			ComposeYAML: "services:\n  a:\n    image: x\n    env:\n      K: {{must}}\n",
			Params:      []ParamSpec{{Name: "must", Type: ParamTypeString, Required: true}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Render(ctx, tpl.ID, map[string]string{}); !errors.Is(err, ErrMissingParam) {
			t.Fatalf("缺必填应 ErrMissingParam,得 %v", err)
		}
		if _, err := svc.Render(ctx, tpl.ID, map[string]string{"must": "ok"}); err != nil {
			t.Fatalf("补齐必填应成功:%v", err)
		}
	})
}

func TestEnsureBuiltinsRefreshesStaleBuiltin(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		svc := newTestService(t, st)
		ctx := context.Background()
		list, err := svc.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var gitea *Template
		for i := range list {
			if list[i].Name == "gitea" {
				gitea = &list[i]
			}
		}
		if gitea == nil {
			t.Fatal("缺 gitea 内置模板")
		}
		// 服务层禁止改内置,直接改 store 模拟旧版 seed 遗留的旧行(如尚未有 ssh_domain 参数)。
		stale := *gitea
		stale.ComposeYAML = "services:\n  gitea:\n    image: gitea/gitea:1.0\n"
		stale.Params = []ParamSpec{{Name: "version", Type: ParamTypeString, Default: "1.0"}}
		if err := NewStore(st.DB).update(ctx, &stale); err != nil {
			t.Fatal(err)
		}
		if err := svc.EnsureBuiltins(ctx); err != nil {
			t.Fatal(err)
		}
		fresh, err := svc.Get(ctx, gitea.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(fresh.ComposeYAML, `SSH_DOMAIN: "{{ssh_domain}}"`) || !strings.Contains(fresh.ComposeYAML, "gitea/gitea:{{version}}") {
			t.Fatalf("内置行应随新版定义刷新:\n%s", fresh.ComposeYAML)
		}
		if fresh.ID != gitea.ID || fresh.CreatedAt.IsZero() {
			t.Fatal("刷新应保留 ID 与创建时间")
		}
		// 内容已一致时再 seed 不报错不空写。
		if err := svc.EnsureBuiltins(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

func TestEnsureBuiltinsSkipsCustomSameName(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		// 先以空内置集合 seed(模拟内置名启用前的老库),用户自建同名模板。
		old := builtins
		builtins = nil
		defer func() { builtins = old }()
		svc := newTestService(t, st)
		ctx := context.Background()
		mine, err := svc.Create(ctx, CreateInput{
			Name: "mine", DisplayName: "我的", ComposeYAML: "services:\n  a:\n    image: x:1\n",
		})
		if err != nil {
			t.Fatal(err)
		}
		// 新版引入同名内置 → seed 让路,不覆盖用户模板。
		builtins = []Template{{Name: "mine", DisplayName: "内置 Mine", ComposeYAML: "services:\n  a:\n    image: y:2\n", Builtin: true}}
		if err := svc.EnsureBuiltins(ctx); err != nil {
			t.Fatal(err)
		}
		got, err := svc.Get(ctx, mine.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Builtin || got.DisplayName != "我的" || !strings.Contains(got.ComposeYAML, "x:1") {
			t.Fatalf("同名自定义模板不应被覆盖:%+v", got)
		}
	})
}

func TestEnsureBuiltinsRetiresRemovedBuiltin(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		ctx := context.Background()
		// 先以仍含 minio 的内置集合 seed(模拟老版本库),再恢复真实定义。
		oldBuiltins, oldRetired := builtins, retiredBuiltins
		defer func() { builtins, retiredBuiltins = oldBuiltins, oldRetired }()
		builtins = append([]Template{{
			Name: "minio", DisplayName: "MinIO", ComposeYAML: "services:\n  minio:\n    image: minio/minio:latest\n", Builtin: true,
		}}, oldBuiltins...)
		retiredBuiltins = nil
		svc := newTestService(t, st)
		builtins, retiredBuiltins = oldBuiltins, []string{"minio"}

		// 升级后 seed → 内置 minio 行被清掉;其余内置不受影响。
		if err := svc.EnsureBuiltins(ctx); err != nil {
			t.Fatal(err)
		}
		list, err := svc.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != len(builtins) {
			t.Fatalf("下架后模板数不符:%d != %d", len(list), len(builtins))
		}
		for _, tpl := range list {
			if tpl.Name == "minio" {
				t.Fatalf("下架内置应被删除:%+v", tpl)
			}
		}

		// 同名自定义模板让路:不被下架逻辑删除。
		mine, err := svc.Create(ctx, CreateInput{
			Name: "minio", DisplayName: "我的 minio", ComposeYAML: "services:\n  a:\n    image: x:1\n",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.EnsureBuiltins(ctx); err != nil {
			t.Fatal(err)
		}
		got, err := svc.Get(ctx, mine.ID)
		if err != nil || got.Builtin {
			t.Fatalf("同名自定义模板不应被下架逻辑删除:%v %+v", err, got)
		}
	})
}
