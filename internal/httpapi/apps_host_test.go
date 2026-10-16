package httpapi

import (
	"testing"

	"github.com/huangchengsir/pipewright/internal/appstore"
)

func TestInjectHostParams(t *testing.T) {
	tpl := &appstore.Template{Params: []appstore.ParamSpec{
		{Name: "advertise_host"}, {Name: "ssh_domain"}, {Name: "port"},
	}}
	// nil 入参 + 模板声明了约定参数 → 全部注入宿主地址。
	got := injectHostParams(nil, tpl, "10.0.0.8")
	if got["advertise_host"] != "10.0.0.8" || got["ssh_domain"] != "10.0.0.8" {
		t.Fatalf("留空应注入宿主地址:%v", got)
	}
	// 显式填写优先;空白串视同留空;无关参数不动。
	got = injectHostParams(map[string]string{"advertise_host": "kafka.example.com", "ssh_domain": "  ", "port": "9092"}, tpl, "10.0.0.8")
	if got["advertise_host"] != "kafka.example.com" || got["ssh_domain"] != "10.0.0.8" || got["port"] != "9092" {
		t.Fatalf("显式值应保留、空白应注入、无关参数不动:%v", got)
	}
	// 模板未声明约定参数 → 原样返回(不为无关模板添参数)。
	same := map[string]string{"port": "3000"}
	if out := injectHostParams(same, &appstore.Template{Params: []appstore.ParamSpec{{Name: "port"}}}, "10.0.0.8"); len(out) != 1 || out["port"] != "3000" {
		t.Fatalf("未声明约定参数不应改动:%v", out)
	}
}
