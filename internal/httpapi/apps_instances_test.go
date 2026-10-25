package httpapi

// 已部署实例聚合枚举(GET /api/servers/apps/instances)的契约测试。
// 直接构造 makeAllAppInstancesHandler(fakes):不触真 SSH / DB,只验证枚举语义 ——
// find 输出 → 栈目录聚合(白名单/排序)、边车解析(损坏降级 legacy)、模板 join(展示名/
// 实例后缀/模板有更新/模板已删)、旧边车无部署时间恒标有更新、无边车旧栈按栈名反推模板
// 作 legacy(反推不到不列)、机器不可达单条诚实告知、PVE 节点排除。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/appstore"
	"github.com/huangchengsir/pipewright/internal/target"
)

// instAppFake:仅实现 List(模板全量;缺某个边车模板 ID 即测 templateGone)。
type instAppFake struct {
	appstore.Service
	tpls []appstore.Template
}

func (f *instAppFake) List(ctx context.Context) ([]appstore.Template, error) {
	return f.tpls, nil
}

// instMachine 描述一台 fake 机器:find 罐头 stdout(边车路径清单)、按栈名回放的边车内容、
// 可达性。pve=true 的机器若被枚举会置 touched(断言 PVE 排除用)。
type instMachine struct {
	srv         *target.Server
	findOut     string
	psOut       string            // docker ps(版本枚举)stdout;空 = 命令未配置
	sidecars    map[string]string // 栈名 → 边车 JSON 原文;无记录 = 读不到(缺失/损坏路径)
	unreachable bool
	touched     bool
}

// instTargetFake:多机 List + Exec 对 find 命令回放该机的罐头 stdout;Download 回放边车原文。
type instTargetFake struct {
	target.Service
	machines []*instMachine
	listErr  error
}

func (f *instTargetFake) List(ctx context.Context) ([]*target.Server, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]*target.Server, 0, len(f.machines))
	for _, m := range f.machines {
		out = append(out, m.srv)
	}
	return out, nil
}

func (f *instTargetFake) find(serverID string) *instMachine {
	for _, m := range f.machines {
		if m.srv.ID == serverID {
			return m
		}
	}
	return nil
}

func (f *instTargetFake) Exec(ctx context.Context, serverID string, cmd []string) (*target.ExecResult, error) {
	m := f.find(serverID)
	if m == nil {
		return nil, errors.New("unknown server " + serverID)
	}
	m.touched = true
	if m.unreachable {
		return nil, errors.New("ssh: dial tcp: connection refused")
	}
	if len(cmd) > 0 && cmd[0] == "docker" && cmd[1] == "ps" {
		return &target.ExecResult{ExitCode: 0, Stdout: m.psOut}, nil
	}
	return &target.ExecResult{ExitCode: 0, Stdout: m.findOut}, nil
}

func (f *instTargetFake) Download(ctx context.Context, serverID string, remotePath string, w io.Writer) (int64, error) {
	m := f.find(serverID)
	if m == nil {
		return 0, errors.New("unknown server " + serverID)
	}
	stack, ok := strings.CutPrefix(remotePath, stacksBaseDir+"/")
	stack, ok2 := strings.CutSuffix(stack, "/"+appParamsFileName)
	if !ok || !ok2 {
		return 0, errors.New("unexpected path " + remotePath)
	}
	body, has := m.sidecars[stack]
	if !has {
		return 0, errors.New("no such file") // 与真实 Download:cat 缺失文件报错一致
	}
	n, _ := io.Copy(w, strings.NewReader(body))
	return n, nil
}

func mustSidecar(t *testing.T, sc appDeploySidecar) string {
	t.Helper()
	raw, err := json.Marshal(sc)
	if err != nil {
		t.Fatalf("marshal sidecar: %v", err)
	}
	return string(raw)
}

// runInstances 打枚举端点并解码 items。
func runInstances(t *testing.T, apps appstore.Service, servers target.Service) []appInstanceDTO {
	t.Helper()
	r := chi.NewRouter()
	r.Get("/servers/apps/instances", makeAllAppInstancesHandler(apps, servers))
	req := httptest.NewRequest(http.MethodGet, "/servers/apps/instances", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Items []appInstanceDTO `json:"items"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("解码响应: %v", err)
	}
	return out.Items
}

// TestAllAppInstances 验证主 join 语义:排序/实例后缀/模板有更新/损坏边车跳过/不可达单条/
// PVE 排除。
func TestAllAppInstances(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) // 部署时间(早于模板更新)
	t1 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC) // 模板 UpdatedAt
	t2 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC) // 升级过的部署(晚于模板更新)
	gitea := appstore.Template{ID: "tpl-1", Name: "gitea", DisplayName: "Gitea", Icon: "🐙", UpdatedAt: t1}

	build1 := &instMachine{
		srv: &target.Server{ID: "srv-1", Name: "build-1", Host: "192.168.1.10"},
		findOut: "/opt/pipewright/stacks/gitea-x/.appstore-params.json\n/opt/pipewright/stacks/gitea-x/docker-compose.yml\n" +
			"/opt/pipewright/stacks/gitea/.appstore-params.json\n/opt/pipewright/stacks/gitea/docker-compose.yml\n" +
			"/opt/pipewright/stacks/gitea-2/.appstore-params.json\n/opt/pipewright/stacks/gitea-2/docker-compose.yml\n",
		sidecars: map[string]string{
			"gitea":   mustSidecar(t, appDeploySidecar{TemplateID: "tpl-1", Params: map[string]string{"host": "git.a"}, DeployedAt: t0}),
			"gitea-2": mustSidecar(t, appDeploySidecar{TemplateID: "tpl-1", Params: map[string]string{"host": "git.b"}, DeployedAt: t2}),
			"gitea-x": "{not json", // 损坏边车 → 降级 legacy
		},
	}
	build2 := &instMachine{
		srv:         &target.Server{ID: "srv-2", Name: "build-2", Host: "192.168.1.11"},
		unreachable: true,
	}
	pve := &instMachine{
		srv: &target.Server{ID: "pve-1", Name: "pve-1", Host: "192.168.1.99", Kind: target.KindPVENode},
	}

	items := runInstances(t, &instAppFake{tpls: []appstore.Template{gitea}},
		&instTargetFake{machines: []*instMachine{build1, build2, pve}})

	if pve.touched {
		t.Fatal("PVE 节点不应被枚举")
	}
	if len(items) != 4 {
		t.Fatalf("items 数量 %d,期望 4(gitea/gitea-2/gitea-x legacy/不可达): %+v", len(items), items)
	}
	// 同机内栈名升序:gitea < gitea-2 < gitea-x;不可达机器垫后(靠 List 顺序)。
	a, b, c, d := items[0], items[1], items[2], items[3]
	if a.StackName != "gitea" || b.StackName != "gitea-2" || c.StackName != "gitea-x" {
		t.Fatalf("栈名或排序不符: %+v %+v %+v", a, b, c)
	}
	if a.ServerID != "srv-1" || a.ServerName != "build-1" || !a.Reachable {
		t.Fatalf("gitea 条目机器信息不符: %+v", a)
	}
	if a.TemplateName != "gitea" || a.DisplayName != "Gitea" || a.Icon != "🐙" || a.TemplateID != "tpl-1" {
		t.Fatalf("gitea 条目模板 join 不符: %+v", a)
	}
	if a.Instance != "" || a.UpgradeAvailable != true || a.Legacy {
		t.Fatalf("gitea(部署早于模板更新)应标 upgradeAvailable 且非 legacy: %+v", a)
	}
	if a.DeployedAt != t0.Format(time.RFC3339) {
		t.Fatalf("deployedAt 应为 RFC3339 的 %s,实得 %q", t0.Format(time.RFC3339), a.DeployedAt)
	}
	if b.Instance != "2" || b.UpgradeAvailable || b.Legacy {
		t.Fatalf("gitea-2 应剥出实例后缀 2 且部署晚于模板更新不标 upgrade: %+v", b)
	}
	// gitea-x:边车损坏 → 降级 legacy(按栈名反推模板),仍可重新部署纳入。
	if !c.Legacy || c.TemplateID != "tpl-1" || c.Instance != "x" || c.UpgradeAvailable || c.DeployedAt != "" {
		t.Fatalf("损坏边车应降级 legacy 且剥出实例后缀: %+v", c)
	}
	if d.Reachable || d.StackName != "" || d.Error == "" || d.ServerID != "srv-2" {
		t.Fatalf("不可达机器应单条 reachable:false 且带 error: %+v", d)
	}
}

// TestAllAppInstancesLegacyAndGone 验证:旧边车(无部署时间)恒标「有更新」且 deployedAt
// 为空(升一次级即建立基准);边车模板 ID 已不在模板库 → templateGone。
func TestAllAppInstancesLegacyAndGone(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	gitea := appstore.Template{ID: "tpl-1", Name: "gitea", DisplayName: "Gitea", UpdatedAt: now}
	m := &instMachine{
		srv: &target.Server{ID: "srv-1", Name: "build-1"},
		findOut: "/opt/pipewright/stacks/gitea/.appstore-params.json\n/opt/pipewright/stacks/gitea/docker-compose.yml\n" +
			"/opt/pipewright/stacks/old-app/.appstore-params.json\n/opt/pipewright/stacks/old-app/docker-compose.yml\n",
		sidecars: map[string]string{
			"gitea":   mustSidecar(t, appDeploySidecar{TemplateID: "tpl-1", Params: map[string]string{}}), // 旧边车:无 deployedAt
			"old-app": mustSidecar(t, appDeploySidecar{TemplateID: "tpl-gone", Params: map[string]string{}}),
		},
	}
	items := runInstances(t, &instAppFake{tpls: []appstore.Template{gitea}}, &instTargetFake{machines: []*instMachine{m}})
	if len(items) != 2 {
		t.Fatalf("items 数量 %d,期望 2: %+v", len(items), items)
	}
	if items[0].DeployedAt != "" || !items[0].UpgradeAvailable {
		t.Fatalf("旧边车无部署时间 → deployedAt 空但恒标有更新(升级一次建立基准): %+v", items[0])
	}
	if !items[1].TemplateGone || items[1].TemplateName != "" || items[1].DisplayName != "" {
		t.Fatalf("模板已删应标 templateGone 且无展示名: %+v", items[1])
	}
}

// TestAllAppInstancesEmpty 验证:find 输出为空(受管目录不存在/无部署)→ 该机零条目。
func TestAllAppInstancesEmpty(t *testing.T) {
	m := &instMachine{srv: &target.Server{ID: "srv-1", Name: "build-1"}, findOut: ""}
	items := runInstances(t, &instAppFake{}, &instTargetFake{machines: []*instMachine{m}})
	if len(items) != 0 {
		t.Fatalf("空 find 输出应零条目: %+v", items)
	}
}

// TestAllAppInstancesLegacyNoSidecar 验证:无边车的旧版部署栈(只有 compose)按栈名反推
// 模板列出为 legacy;反推不到模板的栈(大概率 Stacks 手动部署)不列。
func TestAllAppInstancesLegacyNoSidecar(t *testing.T) {
	gitea := appstore.Template{ID: "tpl-1", Name: "gitea", DisplayName: "Gitea", UpdatedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)}
	m := &instMachine{
		srv: &target.Server{ID: "srv-1", Name: "build-1"},
		findOut: "/opt/pipewright/stacks/gitea/docker-compose.yml\n" +
			"/opt/pipewright/stacks/gitea-old/docker-compose.yml\n" +
			"/opt/pipewright/stacks/manual-stack/docker-compose.yml\n",
		sidecars: map[string]string{}, // 全部无边车
	}
	items := runInstances(t, &instAppFake{tpls: []appstore.Template{gitea}}, &instTargetFake{machines: []*instMachine{m}})
	if len(items) != 2 {
		t.Fatalf("items 数量 %d,期望 2(gitea/gitea-old legacy;manual-stack 不列): %+v", len(items), items)
	}
	if !items[0].Legacy || items[0].StackName != "gitea" || items[0].Instance != "" || items[0].TemplateID != "tpl-1" {
		t.Fatalf("gitea 应为 legacy 且无实例后缀: %+v", items[0])
	}
	if items[0].DisplayName != "Gitea" || items[0].UpgradeAvailable || items[0].DeployedAt != "" {
		t.Fatalf("legacy 条目应带模板展示名且不标更新/时间: %+v", items[0])
	}
	if !items[1].Legacy || items[1].StackName != "gitea-old" || items[1].Instance != "old" {
		t.Fatalf("gitea-old 应为 legacy 且剥出实例后缀 old: %+v", items[1])
	}
}


// ---- 实例版本显示(运行镜像 tag) ----

func TestImagesByProjectAndTag(t *testing.T) {
	m := imagesByProject("mysql|||mysql:8.4\nredis|||redis:7-alpine\nmysql|||mysql:8.4\n|||noise:1\nsolo|||nginx\n")
	if m["mysql"] != "mysql:8.4" || m["redis"] != "redis:7-alpine" || m["solo"] != "nginx" || len(m) != 3 {
		t.Fatalf("imagesByProject:%+v", m)
	}
	if imageVersionTag("mysql:8.4") != "8.4" || imageVersionTag("reg.io/team/app:2.0") != "2.0" ||
		imageVersionTag("nginx") != "" || imageVersionTag("reg.io:5000/app") != "" {
		t.Fatal("imageVersionTag 提取不符")
	}
}

func TestCollectAppInstancesVersion(t *testing.T) {
	tpl := appstore.Template{ID: "t-mysql", Name: "mysql", DisplayName: "MySQL"}
	tf := &instTargetFake{machines: []*instMachine{{
		srv:     &target.Server{ID: "s1", Name: "机A"},
		findOut: "/opt/pipewright/stacks/mysql/docker-compose.yml\n/opt/pipewright/stacks/mysql/.appstore-params.json\n",
		psOut:   "mysql|||mysql:8.4\ngitea|||gitea/gitea:28.1.0\n",
		sidecars: map[string]string{"mysql": mustSidecar(t, appDeploySidecar{TemplateID: "t-mysql"})},
	}}}
	af := &instAppFake{tpls: []appstore.Template{tpl}}
	items := collectAppInstances(context.Background(), tf, tf.machines[0].srv,
		map[string]appstore.Template{tpl.ID: tpl})
	_ = af
	if len(items) != 1 || items[0].Version != "8.4" {
		t.Fatalf("实例版本不符:%+v", items)
	}
}
