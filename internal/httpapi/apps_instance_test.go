package httpapi

// 应用商店部署「实例名」维度的契约测试(复用 apps_register_test.go 的 fakes):
// 栈目录/compose 项目名带实例后缀、响应 name 为生效栈名、非法实例名被拒、
// RustFS 联动登记名带实例后缀、实例留空保持既有单实例语义。

import (
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/objectstore"
)

// execCmdContains 在记录的命令里找一条以空格拼接后包含片段的命令(mkdir/up/probe 都适用)。
func execCmdContains(cmds [][]string, want string) bool {
	for _, cmd := range cmds {
		if strings.Contains(strings.Join(cmd, " "), want) {
			return true
		}
	}
	return false
}

// TestDeployInstanceSuffixesStack:instance=2 → 栈目录与 -p 项目名、compose 上传路径、
// 响应 name 都用 gitea-2。
func TestDeployInstanceSuffixesStack(t *testing.T) {
	apps, servers := rustfsDeployFakes()
	apps.tpl.Name = "gitea"

	out := runDeployBody(t, apps, servers, nil, `{"templateId":"tpl-1","params":{},"instance":"2"}`)
	if !out.OK {
		t.Fatalf("部署应成功: %+v", out)
	}
	if out.Name != "gitea-2" {
		t.Fatalf("响应 name 应为生效栈名 gitea-2,得 %q", out.Name)
	}
	if !execCmdContains(servers.execCmds, "/opt/pipewright/stacks/gitea-2") {
		t.Fatalf("mkdir 应创建带实例后缀的目录: %v", servers.execCmds)
	}
	if !execCmdContains(servers.execCmds, "-p gitea-2") {
		t.Fatalf("compose up 应用项目名 gitea-2: %v", servers.execCmds)
	}
	found := false
	for _, p := range servers.uploads {
		if p == "/opt/pipewright/stacks/gitea-2/docker-compose.yml" {
			found = true
		}
	}
	if !found {
		t.Fatalf("compose 应上传到带实例后缀的目录: %v", servers.uploads)
	}
}

// TestDeployInstanceEmptyKeepsTemplateName:实例留空 → 行为与既有单实例部署完全一致。
func TestDeployInstanceEmptyKeepsTemplateName(t *testing.T) {
	apps, servers := rustfsDeployFakes()
	apps.tpl.Name = "gitea"

	out := runDeployBody(t, apps, servers, nil, `{"templateId":"tpl-1","params":{},"instance":"  "}`)
	if !out.OK || out.Name != "gitea" {
		t.Fatalf("留空(含纯空白)应退回模板名: %+v", out)
	}
	if !execCmdContains(servers.execCmds, "-p gitea") || execCmdContains(servers.execCmds, "gitea-") {
		t.Fatalf("不应出现实例后缀: %v", servers.execCmds)
	}
}

// TestDeployInstanceInvalidRejected:非法实例名(shell 元字符/以 - 开头)→ 400 invalid_app_deploy。
func TestDeployInstanceInvalidRejected(t *testing.T) {
	for _, instance := range []string{"-lead", "a;rm", "带空格 x", "../evil"} {
		apps, servers := rustfsDeployFakes()
		body := `{"templateId":"tpl-1","params":{},"instance":"` + instance + `"}`
		code, raw := runDeployRaw(t, apps, servers, nil, body)
		if code != 400 || !strings.Contains(raw, "invalid_app_deploy") {
			t.Fatalf("实例名 %q 应 400 invalid_app_deploy,得 %d %s", instance, code, raw)
		}
	}
}

// TestDeployRustFSInstanceNamesRegister:rustfs 多副本 → 对象存储登记名带实例后缀区分。
func TestDeployRustFSInstanceNamesRegister(t *testing.T) {
	apps, servers := rustfsDeployFakes()
	objStores := &deployObjStoreFake{res: &objectstore.RegisterResult{Provider: &objectstore.Provider{ID: "os-1"}}}

	out := runDeployBody(t, apps, servers, objStores, `{"templateId":"tpl-1","params":{},"instance":"2"}`)
	if !out.OK || !out.ObjectStoreRegistered {
		t.Fatalf("rustfs 副本部署应成功并登记: %+v", out)
	}
	if in := objStores.calls[0]; in.Name != "RustFS · build-1 · 2" {
		t.Fatalf("登记名应带实例后缀,得 %q", in.Name)
	}
}
