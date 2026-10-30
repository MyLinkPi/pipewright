package aptcacherng

import (
	"context"
	"io"
	"strings"
	"testing"
)

// fakeMachine 是 MachineRunner 的测试替身:按命令脚本化响应,记录 Upload 内容(不碰真实 /etc/apt)。
type fakeMachine struct {
	execStep  func(cmd []string) (string, string, int, error)
	uploads   map[string]string
	execs     [][]string
	uploadErr error
}

func (m *fakeMachine) Exec(_ context.Context, cmd []string) (string, string, int, error) {
	m.execs = append(m.execs, cmd)
	if m.execStep != nil {
		return m.execStep(cmd)
	}
	return "", "", 0, nil
}

func (m *fakeMachine) Upload(_ context.Context, content io.Reader, remotePath string) error {
	if m.uploadErr != nil {
		return m.uploadErr
	}
	b, _ := io.ReadAll(content)
	if m.uploads == nil {
		m.uploads = map[string]string{}
	}
	m.uploads[remotePath] = string(b)
	return nil
}

// machineAptDumpPointsAt 造一台 apt-get 可用、apt-config dump 指向该地址的机器(树状形态)。
func machineAptDumpPointsAt(addr string) *fakeMachine {
	return &fakeMachine{execStep: func(cmd []string) (string, string, int, error) {
		j := strings.Join(cmd, " ")
		switch {
		case strings.Contains(j, "command -v apt-get"):
			return "/usr/bin/apt-get", "", 0, nil
		case strings.Contains(j, "apt-config dump"):
			return "Acquire\n{\n http\n {\n  Proxy \"http://" + addr + "/\";\n };\n};", "", 0, nil
		default:
			return "", "", 0, nil
		}
	}}
}

func TestProxyConfContentPlainAndTLS(t *testing.T) {
	plain := &Config{Enabled: true, ExternalAddr: "ctrl", Port: 3142}
	got := string(proxyConfContent(plain))
	if !strings.Contains(got, `Acquire::http::Proxy "http://ctrl:3142";`) ||
		!strings.Contains(got, `Acquire::https::Proxy "http://ctrl:3142";`) {
		t.Fatalf("明文片段不符:\n%s", got)
	}
	tls := &Config{Enabled: true, ExternalAddr: "apt.example.com", Port: 3142, TLSCertID: "cert-1"}
	gotTLS := string(proxyConfContent(tls))
	if !strings.Contains(gotTLS, `Acquire::http::Proxy "https://apt.example.com:3142";`) {
		t.Fatalf("TLS 片段应为 https:\n%s", gotTLS)
	}
}

func TestApplyProxyLocalSuccess(t *testing.T) {
	db := storetestOpen(t)
	machine := machineAptDumpPointsAt("192.168.1.10:3142")
	h := New(db, nil, Options{BaseDir: t.TempDir(), Runner: &scriptRunner{}, LocalMachine: machine})
	ctx := context.Background()
	if _, err := h.Save(ctx, SaveInput{Enabled: true, ExternalAddr: "192.168.1.10"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	results, err := h.ApplyProxy(ctx, nil, true)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(results) != 1 || !results[0].OK || !results[0].IsLocal {
		t.Fatalf("本机下发应成功: %+v", results)
	}
	if results[0].BackupPath == "" {
		t.Fatalf("应回显备份路径: %+v", results[0])
	}
	// 上传到临时名,内容含目标代理值。
	up, ok := machine.uploads[proxyConfPath+".tmp.pipewright"]
	if !ok || !strings.Contains(up, "http://192.168.1.10:3142") {
		t.Fatalf("上传内容不符: ok=%v q=%q", ok, up)
	}
	// 原子替换用 mv -f,验证用 apt-config dump。
	j := joined(machine.execs)
	if !strings.Contains(j, "mv -f "+proxyConfPath+".tmp.pipewright "+proxyConfPath) {
		t.Fatalf("应原子替换正式路径:\n%s", j)
	}
	if !strings.Contains(j, "apt-config dump") {
		t.Fatalf("应 apt-config 验证:\n%s", j)
	}
}

func TestApplyProxyNonDebianFails(t *testing.T) {
	db := storetestOpen(t)
	machine := &fakeMachine{execStep: func(cmd []string) (string, string, int, error) {
		if strings.Contains(strings.Join(cmd, " "), "command -v apt-get") {
			return "", "", 1, nil // 无 apt-get
		}
		return "", "", 0, nil
	}}
	h := New(db, nil, Options{BaseDir: t.TempDir(), Runner: &scriptRunner{}, LocalMachine: machine})
	ctx := context.Background()
	if _, err := h.Save(ctx, SaveInput{Enabled: true, ExternalAddr: "192.168.1.10"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	results, err := h.ApplyProxy(ctx, nil, true)
	if err != nil || len(results) != 1 || results[0].OK {
		t.Fatalf("非 Debian 应失败: err=%v res=%+v", err, results)
	}
	// 探测失败绝不开写文件。
	if len(machine.uploads) != 0 {
		t.Fatalf("无 apt-get 不应上传任何文件: %+v", machine.uploads)
	}
}

func TestApplyProxyVerifyMismatch(t *testing.T) {
	db := storetestOpen(t)
	// apt-config dump 指向别的地址 → 判定未生效。
	machine := machineAptDumpPointsAt("somewhere-else:9999")
	h := New(db, nil, Options{BaseDir: t.TempDir(), Runner: &scriptRunner{}, LocalMachine: machine})
	ctx := context.Background()
	if _, err := h.Save(ctx, SaveInput{Enabled: true, ExternalAddr: "192.168.1.10"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	results, err := h.ApplyProxy(ctx, nil, true)
	if err != nil || len(results) != 1 || results[0].OK {
		t.Fatalf("代理值不符应判失败: err=%v res=%+v", err, results)
	}
	if !strings.Contains(results[0].Error, "apt-config") {
		t.Fatalf("错误应指向生效验证: %q", results[0].Error)
	}
}

func TestApplyProxyMissingTarget(t *testing.T) {
	db := storetestOpen(t)
	h := New(db, nil, Options{BaseDir: t.TempDir(), Runner: &scriptRunner{}, LocalMachine: machineAptDumpPointsAt("192.168.1.10:3142")})
	ctx := context.Background()
	if _, err := h.Save(ctx, SaveInput{Enabled: true, ExternalAddr: "192.168.1.10"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	// targetSvc 为 nil:未知服务器 id 仍占一个失败结果位(不静默吞掉)。
	results, err := h.ApplyProxy(ctx, []string{"srv-404"}, false)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(results) != 1 || results[0].OK || results[0].TargetID != "srv-404" {
		t.Fatalf("未知目标应失败且占位: %+v", results)
	}
}

func TestApplyProxyDisabled(t *testing.T) {
	db := storetestOpen(t)
	h := New(db, nil, Options{BaseDir: t.TempDir(), Runner: &scriptRunner{}, LocalMachine: &fakeMachine{}})
	if _, err := h.ApplyProxy(context.Background(), nil, true); err != ErrDisabled {
		t.Fatalf("未启用应 ErrDisabled, got %v", err)
	}
}

func TestApplyProxyDedupTargets(t *testing.T) {
	db := storetestOpen(t)
	machine := machineAptDumpPointsAt("192.168.1.10:3142")
	h := New(db, nil, Options{BaseDir: t.TempDir(), Runner: &scriptRunner{}, LocalMachine: machine})
	ctx := context.Background()
	if _, err := h.Save(ctx, SaveInput{Enabled: true, ExternalAddr: "192.168.1.10"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	// local + 重复 id 去重:includeLocal 已含 "local",serverIDs 再塞 "local" 与重复值。
	results, err := h.ApplyProxy(ctx, []string{"local", "local"}, true)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("重复/本机应去重为 1, got %d: %+v", len(results), results)
	}
}

func TestInspectProxyLocal(t *testing.T) {
	db := storetestOpen(t)
	const want = "http://192.168.1.10:3142/"
	machine := &fakeMachine{execStep: func(cmd []string) (string, string, int, error) {
		j := strings.Join(cmd, " ")
		switch {
		case strings.Contains(j, "apt-config dump"):
			return "Acquire\n{\n http\n {\n  Proxy \"" + want + "\";\n };\n};", "", 0, nil
		case strings.Contains(j, "Acquire::http::Proxy/f"):
			return want, "", 0, nil
		case strings.Contains(j, "Acquire::https::Proxy/f"):
			return want, "", 0, nil
		case strings.Contains(j, "cat "+proxyConfPath):
			return `Acquire::http::Proxy "http://192.168.1.10:3142";`, "", 0, nil
		default:
			return "", "", 0, nil
		}
	}}
	h := New(db, nil, Options{BaseDir: t.TempDir(), Runner: &scriptRunner{}, LocalMachine: machine})
	ins, err := h.InspectProxy(context.Background(), LocalTargetID)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !ins.IsLocal || ins.TargetName != "控制机本机" {
		t.Fatalf("应为本机巡检: %+v", ins)
	}
	if ins.ProxyHTTP != want {
		t.Fatalf("应解析出 http 代理值 %q, got %q", want, ins.ProxyHTTP)
	}
	if ins.ConfFile == "" {
		t.Fatalf("应回显片段原文: %+v", ins)
	}
}

func TestAptDumpPointsAt(t *testing.T) {
	out := "Acquire\n{\n http\n {\n  Proxy \"http://ctrl:3142/\";\n };\n};"
	if !aptDumpPointsAt(out, "ctrl:3142") {
		t.Fatalf("含地址 token 应命中")
	}
	if aptDumpPointsAt(out, "other:1") {
		t.Fatalf("不含目标地址不应命中")
	}
	if aptDumpPointsAt(out, "") {
		t.Fatalf("空地址不应命中")
	}
}
