//go:build !windows && !linux && !darwin

// svc_unsupported.go 为其余类 Unix 平台(FreeBSD 等)提供占位:保持可编译,服务操作
// 明确拒绝(未实现 systemd/launchd 管理器)。前台形态(console/ui)仍可用。
package main

import "fmt"

// noopManager 是"无服务管理器"占位:一切操作拒绝。
type noopManager struct{}

func newServiceManager() serviceManager { return noopManager{} }

func (noopManager) available() bool { return false }

func (noopManager) install(_ string) error { return errNoServiceManager() }
func (noopManager) start() error           { return errNoServiceManager() }
func (noopManager) stop() error            { return errNoServiceManager() }
func (noopManager) uninstall() error       { return errNoServiceManager() }

func (noopManager) state() string { return "NOT_INSTALLED" }

func (noopManager) binaryPath() (string, error) { return "", errNoServiceManager() }

func (noopManager) autoStart() bool { return false }

func (noopManager) setAutoStart(bool) error { return errNoServiceManager() }

func errNoServiceManager() error {
	return fmt.Errorf("当前平台无可用服务管理器(仅支持 Linux systemd / macOS launchd);可用前台形态:pwclient console")
}
