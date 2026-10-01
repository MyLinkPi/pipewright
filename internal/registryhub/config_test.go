package registryhub

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/storetest"
)

// newTestHub 构造测试 Hub(sqlite 全量迁移 + fake 本机执行器/文件通道,不触网不碰真 docker)。
func newTestHub(t *testing.T, opts Options) *Hub {
	t.Helper()
	db := storetest.OpenDB(t)
	return New(db, nil, opts)
}

func TestConfigLazyDefault(t *testing.T) {
	h := newTestHub(t, Options{})
	cfg, err := h.Get(context.Background())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if cfg.Enabled || cfg.ExternalAddr != "" || cfg.UpstreamURL != DefaultUpstreamURL ||
		cfg.ArtifactPort != DefaultArtifactPort || cfg.CachePort != DefaultCachePort {
		t.Fatalf("惰性默认不符: %+v", cfg)
	}
	if cfg.UpdatedAt != nil {
		t.Fatalf("默认 updatedAt 应 nil")
	}
}

func TestConfigSaveRoundtripAndReload(t *testing.T) {
	db := storetest.OpenDB(t)
	h := New(db, nil, Options{})
	cfg, err := h.Save(context.Background(), SaveInput{
		Enabled: true, ExternalAddr: "192.168.1.10",
		ArtifactPort: 0, CachePort: 0, // 0 → 默认兜底
		KeepPerProject: 10, MaxAgeDays: -3, // 负值归一 0
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if !cfg.Enabled || cfg.ExternalAddr != "192.168.1.10" || cfg.ArtifactPort != 5000 || cfg.CachePort != 5001 {
		t.Fatalf("保存结果不符: %+v", cfg)
	}
	if cfg.KeepPerProject != 10 || cfg.MaxAgeDays != 0 {
		t.Fatalf("retention 归一不符: %+v", cfg)
	}
	if cfg.UpdatedAt == nil {
		t.Fatalf("保存后 updatedAt 应非 nil")
	}
	// 同库新实例重新加载,验证持久化。
	h2 := New(db, nil, Options{})
	cfg2, err := h2.Get(context.Background())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !cfg2.Enabled || cfg2.ExternalAddr != "192.168.1.10" || cfg2.KeepPerProject != 10 {
		t.Fatalf("重载不符: %+v", cfg2)
	}
}

func TestConfigSaveValidation(t *testing.T) {
	cases := []struct {
		name string
		in   SaveInput
		want error
	}{
		{"启用但无地址", SaveInput{Enabled: true}, ErrInvalidAddr},
		{"地址带协议", SaveInput{Enabled: true, ExternalAddr: "http://1.2.3.4"}, ErrInvalidAddr},
		{"地址带端口", SaveInput{Enabled: true, ExternalAddr: "1.2.3.4:5000"}, ErrInvalidAddr},
		{"地址带路径", SaveInput{Enabled: true, ExternalAddr: "host/path"}, ErrInvalidAddr},
		{"未启用空地址合法", SaveInput{Enabled: false, ExternalAddr: ""}, nil},
		{"上游缺scheme", SaveInput{Enabled: true, ExternalAddr: "h1", UpstreamURL: "1.2.3.4"}, ErrInvalidUpstream},
		{"上游非法scheme", SaveInput{Enabled: true, ExternalAddr: "h1", UpstreamURL: "ftp://x"}, ErrInvalidUpstream},
		{"端口越界", SaveInput{Enabled: true, ExternalAddr: "h1", ArtifactPort: 70000}, ErrInvalidPort},
		{"端口相同", SaveInput{Enabled: true, ExternalAddr: "h1", ArtifactPort: 5000, CachePort: 5000}, ErrInvalidPort},
		{"合法自定义端口", SaveInput{Enabled: true, ExternalAddr: "h1", ArtifactPort: 6000, CachePort: 6001}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHub(t, Options{})
			_, err := h.Save(context.Background(), tc.in)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("应合法,得 %v", err)
				}
				return
			}
			if err == nil || err.Error() != tc.want.Error() {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
}

func TestAddrHelpers(t *testing.T) {
	cfg := &Config{ExternalAddr: "10.0.0.5", ArtifactPort: 5000, CachePort: 5001}
	if got := cfg.ArtifactAddr(); got != "10.0.0.5:5000" {
		t.Fatalf("artifact addr = %q", got)
	}
	if got := cfg.CacheAddr(); got != "10.0.0.5:5001" {
		t.Fatalf("cache addr = %q", got)
	}
	if got := mirrorURL(cfg); got != "http://10.0.0.5:5001" {
		t.Fatalf("mirror url = %q", got)
	}
}

func TestDaemonJSONContent(t *testing.T) {
	cfg := &Config{ExternalAddr: "ctrl", ArtifactPort: 5000, CachePort: 5001}
	got := string(daemonJSONContent(cfg))
	want := `{
  "insecure-registries": [
    "ctrl:5000",
    "ctrl:5001"
  ],
  "registry-mirrors": [
    "http://ctrl:5001"
  ]
}`
	if got != want+"\n" {
		t.Fatalf("daemon.json 内容不符:\n%s\nwant:\n%s", got, want)
	}
}

// fakeLocalMachine 是可控的 MachineRunner(按命令前缀脚本化响应;记录全部命令与上传内容)。
type fakeLocalMachine struct {
	responses []fakeResp // 顺序消费;越界默认 0/""/空
	commands  [][]string
	uploads   map[string]string
	index     int
}

type fakeResp struct {
	stdout      string
	stderr      string
	exitCode    int
	uploadFirst bool // 该响应供 Upload 消费(Exec/Upload 共用同一顺序脚本)
}

func (f *fakeLocalMachine) Exec(_ context.Context, cmd []string) (string, string, int, error) {
	f.commands = append(f.commands, cmd)
	if f.index < len(f.responses) && !f.responses[f.index].uploadFirst {
		r := f.responses[f.index]
		f.index++
		return r.stdout, r.stderr, r.exitCode, nil
	}
	if f.index < len(f.responses) {
		// 脚本位是 upload 预留:按未匹配处理(返回 0)。
		return "", "", 0, nil
	}
	return "", "", 0, nil
}

func (f *fakeLocalMachine) Upload(_ context.Context, content io.Reader, remotePath string) error {
	b, _ := io.ReadAll(content)
	if f.uploads == nil {
		f.uploads = map[string]string{}
	}
	f.uploads[remotePath] = string(b)
	if f.index < len(f.responses) && f.responses[f.index].uploadFirst {
		f.index++
	}
	return nil
}

func TestApplyDaemonLocalHappyPath(t *testing.T) {
	machine := &fakeLocalMachine{responses: []fakeResp{
		{},                               // 1 探测 docker
		{},                               // 2 备份
		{uploadFirst: true},              // 3 上传临时文件
		{},                               // chmod
		{},                               // 4 dockerd --validate(0)
		{},                               // 5 mv
		{},                               // 6 systemctl restart docker
		{stdout: `["http://ctrl:5001"]`}, // 7 docker info 验证
	}}
	h := newTestHub(t, Options{LocalMachine: machine})
	if _, err := h.Save(context.Background(), SaveInput{Enabled: true, ExternalAddr: "ctrl"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	results, err := h.ApplyDaemon(context.Background(), nil, true)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(results) != 1 || !results[0].OK || results[0].Error != "" {
		t.Fatalf("结果应成功: %+v", results)
	}
	if results[0].TargetID != LocalTargetID || !results[0].IsLocal {
		t.Fatalf("目标应为控制机本机: %+v", results[0])
	}
	if results[0].BackupPath == "" {
		t.Fatalf("应记录备份路径")
	}
	// 上传的临时文件内容即生成的 daemon.json。
	if got := machine.uploads["/etc/docker/daemon.json.tmp.pipewright"]; got != string(daemonJSONContent(&Config{ExternalAddr: "ctrl", ArtifactPort: 5000, CachePort: 5001})) {
		t.Fatalf("上传内容不符: %q", got)
	}
	// 关键命令齐备:重启 + 验证。
	joined := ""
	for _, c := range machine.commands {
		joined += strings.Join(c, " ") + "\n"
	}
	if !strings.Contains(joined, "systemctl restart docker") || !strings.Contains(joined, "docker info --format") {
		t.Fatalf("命令序列缺关键步骤: %s", joined)
	}
}

func TestApplyDaemonNoDocker(t *testing.T) {
	machine := &fakeLocalMachine{responses: []fakeResp{{exitCode: 1}}}
	h := newTestHub(t, Options{LocalMachine: machine})
	if _, err := h.Save(context.Background(), SaveInput{Enabled: true, ExternalAddr: "ctrl"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	results, err := h.ApplyDaemon(context.Background(), nil, true)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if results[0].OK || results[0].Error == "" {
		t.Fatalf("无 docker 应失败: %+v", results[0])
	}
	if len(machine.uploads) != 0 {
		t.Fatalf("无 docker 时不得写任何文件")
	}
}

func TestApplyDaemonValidateFailureAborts(t *testing.T) {
	machine := &fakeLocalMachine{responses: []fakeResp{
		{},                                      // 探测
		{},                                      // 备份
		{uploadFirst: true},                     // 上传
		{},                                      // chmod
		{exitCode: 1, stderr: "invalid config"}, // dockerd --validate 失败
	}}
	h := newTestHub(t, Options{LocalMachine: machine})
	if _, err := h.Save(context.Background(), SaveInput{Enabled: true, ExternalAddr: "ctrl"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	results, _ := h.ApplyDaemon(context.Background(), nil, true)
	if results[0].OK {
		t.Fatalf("校验失败应中止: %+v", results[0])
	}
	if !strings.Contains(results[0].Error, "校验未通过") {
		t.Fatalf("错误应提示校验失败: %q", results[0].Error)
	}
	joined := ""
	for _, c := range machine.commands {
		joined += strings.Join(c, " ") + "\n"
	}
	if strings.Contains(joined, "systemctl restart docker") || strings.Contains(joined, "mv ") {
		t.Fatalf("校验失败不得替换/重启: %s", joined)
	}
}

func TestApplyDaemonRestartFailure(t *testing.T) {
	machine := &fakeLocalMachine{responses: []fakeResp{
		{}, {}, {uploadFirst: true}, {}, {},
		{}, // mv
		{exitCode: 1, stderr: "Unit docker.service not found"}, // 重启失败
	}}
	h := newTestHub(t, Options{LocalMachine: machine})
	if _, err := h.Save(context.Background(), SaveInput{Enabled: true, ExternalAddr: "ctrl"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	results, _ := h.ApplyDaemon(context.Background(), nil, true)
	if results[0].OK || !strings.Contains(results[0].Error, "重启 docker 失败") {
		t.Fatalf("重启失败应报错: %+v", results[0])
	}
}

func TestApplyDaemonMirrorNotEffective(t *testing.T) {
	machine := &fakeLocalMachine{responses: []fakeResp{
		{}, {}, {uploadFirst: true}, {}, {}, {},
		{},                                   // 重启成功
		{stdout: `["https://other.mirror"]`}, // 镜像源未生效
	}}
	h := newTestHub(t, Options{LocalMachine: machine})
	if _, err := h.Save(context.Background(), SaveInput{Enabled: true, ExternalAddr: "ctrl"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	results, _ := h.ApplyDaemon(context.Background(), nil, true)
	if results[0].OK || !strings.Contains(results[0].Error, "未生效") {
		t.Fatalf("镜像源未生效应报错: %+v", results[0])
	}
}

func TestApplyDaemonDisabled(t *testing.T) {
	h := newTestHub(t, Options{})
	if _, err := h.ApplyDaemon(context.Background(), nil, true); err == nil || err != ErrDisabled {
		t.Fatalf("未启用应 ErrDisabled, got %v", err)
	}
}

func TestApplyDaemonExplicitTargetsOnly(t *testing.T) {
	// 本机 + 无服务器:显式空目标列表 + 不含本机 → 无目标;范围绝不自行扩大。
	h := newTestHub(t, Options{LocalMachine: &fakeLocalMachine{}})
	if _, err := h.Save(context.Background(), SaveInput{Enabled: true, ExternalAddr: "ctrl"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	results, err := h.ApplyDaemon(context.Background(), []string{"srv-404"}, false)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(results) != 1 || results[0].OK || results[0].TargetID != "srv-404" {
		t.Fatalf("不存在的服务器应占位失败: %+v", results)
	}
}

func TestInspectDaemonLocal(t *testing.T) {
	machine := &fakeLocalMachine{responses: []fakeResp{
		{stdout: `["http://ctrl:5001"]`},                      // docker info
		{stdout: `{"registry-mirrors":["http://ctrl:5001"]}`}, // cat daemon.json
	}}
	h := newTestHub(t, Options{LocalMachine: machine})
	ins, err := h.InspectDaemon(context.Background(), LocalTargetID)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !ins.IsLocal || len(ins.Mirrors) != 1 || ins.Mirrors[0] != "http://ctrl:5001" || ins.Error != "" {
		t.Fatalf("巡检结果不符: %+v", ins)
	}
	if !strings.Contains(ins.DaemonJSON, "registry-mirrors") {
		t.Fatalf("daemon.json 原文应回显: %q", ins.DaemonJSON)
	}
}
