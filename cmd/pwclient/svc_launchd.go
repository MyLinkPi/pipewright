//go:build darwin

// svc_launchd.go 是 macOS 的服务管理器实现:LaunchDaemon(/Library/LaunchDaemons/
// com.pipewright.client.plist)。RunAtLoad + KeepAlive 令其开机常驻、异常退出自动拉起;
// 停止经 bootout 卸载(否则 KeepAlive 会立即重启),升级停服用 bootout 可靠。
//
// bootout/bootstrap 采用现代域语法(macOS 10.11+);对未加载/已加载的幂等错误做容错。
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const (
	launchdLabel   = "com.pipewright.client"
	launchdPlist   = "/Library/LaunchDaemons/" + launchdLabel + ".plist"
	launchdService = "system/" + launchdLabel
	launchdLogPath = "/var/log/pipewright-client.log"
)

// launchdManager 经 launchctl 管理 LaunchDaemon。
type launchdManager struct{}

func newServiceManager() serviceManager { return launchdManager{} }

// available launchd 恒在(所有 macOS 都有);仅校验 launchctl 可执行。
func (launchdManager) available() bool {
	_, err := exec.LookPath("launchctl")
	return err == nil
}

func (launchdManager) install(exePath string) error {
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>service</string>
        <string>run</string>
    </array>
    <key>RunAtLoad</key><true/>
    <key>KeepAlive</key><true/>
    <key>StandardOutPath</key><string>%s</string>
    <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, launchdLabel, xmlEscape(exePath), launchdLogPath, launchdLogPath)
	if err := os.WriteFile(launchdPlist, []byte(plist), 0o644); err != nil {
		return fmt.Errorf("写入 %s 失败:%w", launchdPlist, err)
	}
	return launchctlBootstrap()
}

func (launchdManager) start() error {
	// 已加载则 kickstart 重启;未加载则 bootstrap 载入(RunAtLoad 立即启动)。
	if launchdPrintOK() {
		if _, err := runLaunchctl("kickstart", "-k", launchdService); err == nil {
			return nil
		}
	}
	return launchctlBootstrap()
}

// stop 卸载任务(bootout):KeepAlive 任务须卸载才不会被自动拉起。未加载视为已停。
func (launchdManager) stop() error {
	out, err := runLaunchctl("bootout", launchdService)
	if err != nil && !isBenign(out) {
		return fmt.Errorf("停止服务失败:%v", err)
	}
	return nil
}

func (m launchdManager) uninstall() error {
	_ = m.stop()
	if err := os.Remove(launchdPlist); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除 %s 失败:%w", launchdPlist, err)
	}
	return nil
}

// state 归一(RUNNING/STOPPED/NOT_INSTALLED/UNKNOWN)。
func (m launchdManager) state() string {
	out, err := runLaunchctl("print", launchdService)
	if err != nil {
		// 未加载:plist 在 → 已装未运行;不在 → 未安装。
		if _, serr := os.Stat(launchdPlist); serr != nil {
			return "NOT_INSTALLED"
		}
		return "STOPPED"
	}
	if strings.Contains(string(out), "state = running") {
		return "RUNNING"
	}
	return "STOPPED"
}

// binaryPath 从 plist 的 ProgramArguments 取 exe;未安装返回错误。
func (launchdManager) binaryPath() (string, error) {
	raw, err := os.ReadFile(launchdPlist)
	if err != nil {
		return "", fmt.Errorf("服务未安装:%w", err)
	}
	if exe := exeFromPlist(string(raw)); exe != "" {
		return exe, nil
	}
	return "", fmt.Errorf("plist 缺少 ProgramArguments")
}

// autoStart 以 plist 中 RunAtLoad 的实际值为准(禁用后 plist 仍在位,但开机不再拉起;
// 缺 RunAtLoad 键时 launchd 默认不在加载时启动,如实返回 false)。
func (launchdManager) autoStart() bool {
	raw, err := os.ReadFile(launchdPlist)
	if err != nil {
		return false
	}
	return plistBoolAfter(string(raw), "RunAtLoad") == "<true/>"
}

// setAutoStart 收敛 RunAtLoad 与 KeepAlive 并重载任务使其生效(off 时同时停服)。
// 两个键必须一并改:KeepAlive=true 时 launchd 无论 RunAtLoad 为何都会在任务加载时
// (含每次开机)拉起进程,只改 RunAtLoad 的"禁用"跨重启无效。
func (m launchdManager) setAutoStart(on bool) error {
	raw, err := os.ReadFile(launchdPlist)
	if err != nil {
		return fmt.Errorf("服务未安装,无法设置自启:%w", err)
	}
	want := "<false/>"
	if on {
		want = "<true/>"
	}
	text := string(raw)
	// 替换各 key 后**最近**的值标签(取 true/false 中更靠前者;扫到更远的同值标签
	// 会错改成后续键的值,例如改 RunAtLoad 时命中 KeepAlive 的 <true/>)。纯函数在
	// plist_bool.go(全平台可测)。
	text, _ = setPlistBoolAfter(text, "RunAtLoad", want)
	text, _ = setPlistBoolAfter(text, "KeepAlive", want)
	if err := os.WriteFile(launchdPlist, []byte(text), 0o644); err != nil {
		return fmt.Errorf("改写 plist 失败:%w", err)
	}
	_ = m.stop()
	if on {
		return launchctlBootstrap()
	}
	return nil
}

// launchctlBootstrap 载入并启动任务(RunAtLoad);已加载视为成功(幂等)。
func launchctlBootstrap() error {
	out, err := runLaunchctl("bootstrap", "system", launchdPlist)
	if err != nil && !isBenign(out) {
		// 回退旧语法(load -w)。
		out2, err2 := runLaunchctl("load", "-w", launchdPlist)
		if err2 != nil && !isBenign(out2) {
			return fmt.Errorf("注册服务失败:%v(%s)", err, firstLine(string(out)))
		}
	}
	return nil
}

// launchdPrintOK 任务是否已加载到 launchd。
func launchdPrintOK() bool {
	_, err := runLaunchctl("print", launchdService)
	return err == nil
}

// isBenign launchctl 对"未加载/已加载"的幂等错误判定(基于合并输出文本,不同版本
// 措辞不一;CombinedOutput 的 error 仅为 exit status,真实原因在 out 中)。
func isBenign(out []byte) bool {
	s := strings.ToLower(string(out))
	return strings.Contains(s, "not loaded") || strings.Contains(s, "no such") ||
		strings.Contains(s, "already") || strings.Contains(s, "501") ||
		strings.Contains(s, "3:") || strings.Contains(s, "already bootstrap")
}

func runLaunchctl(args ...string) ([]byte, error) {
	cmd := exec.Command("launchctl", args...)
	return cmd.CombinedOutput()
}

// exeFromPlist 从 plist 文本取 ProgramArguments 数组首个 string(exe 路径)。
func exeFromPlist(text string) string {
	i := strings.Index(text, "<key>ProgramArguments</key>")
	if i < 0 {
		return ""
	}
	rest := text[i:]
	arr := strings.Index(rest, "<array>")
	if arr < 0 {
		return ""
	}
	rest = rest[arr:]
	open := strings.Index(rest, "<string>")
	if open < 0 {
		return ""
	}
	rest = rest[open+len("<string>"):]
	close := strings.Index(rest, "</string>")
	if close < 0 {
		return ""
	}
	return xmlUnescape(strings.TrimSpace(rest[:close]))
}

// xmlEscape 转义 plist 文本值(仅需 & < >,"路径场景足够)。
func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	return strings.ReplaceAll(s, ">", "&gt;")
}

// xmlUnescape 反转义(与 xmlEscape 对应)。
func xmlUnescape(s string) string {
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	return strings.ReplaceAll(s, "&amp;", "&")
}
