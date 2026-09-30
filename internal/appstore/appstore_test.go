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
