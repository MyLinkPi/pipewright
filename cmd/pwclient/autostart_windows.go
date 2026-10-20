// autostart_windows.go 实现托盘 GUI 的登录自启:写/删当前用户(HKCU)Run 注册表项,
// 无需管理员权限。注意与组网服务的开机自启(服务管理器,机器级)相互独立:
// 服务不登录也已组网,这里的自启只决定「登录后托盘图标是否出现」。
//
//go:build windows

package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows/registry"
)

const (
	// autoStartValueName 与清单 identity(pipewright-client)保持一致。
	autoStartValueName = "pipewright-client"
	runKeyPath         = `Software\Microsoft\Windows\CurrentVersion\Run`
)

// autoStartCommand 返回自启命令:带引号的当前 exe 路径 + --minimized(托盘形态启动)。
func autoStartCommand() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("定位自身路径失败:%w", err)
	}
	// --minimized:开机自启以托盘形态启动(不弹主窗口);双击启动无此参数,正常显示。
	return `"` + exe + `" --minimized`, nil
}

// autoStartValue 读取已存的自启命令;未配置时返回空串。
func autoStartValue() string {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, err := k.GetStringValue(autoStartValueName)
	if err != nil {
		return ""
	}
	return v
}

// autoStartEnabled 报告当前用户是否配置了登录自启。
func autoStartEnabled() bool { return autoStartValue() != "" }

// setAutoStart 写入/删除自启项;删除不存在的项视为成功(便于清理残留)。
func setAutoStart(on bool) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("打开注册表 Run 键失败:%w", err)
	}
	defer k.Close()
	if !on {
		if err := k.DeleteValue(autoStartValueName); err != nil && err != registry.ErrNotExist {
			return fmt.Errorf("删除自启项失败:%w", err)
		}
		return nil
	}
	cmd, err := autoStartCommand()
	if err != nil {
		return err
	}
	if err := k.SetStringValue(autoStartValueName, cmd); err != nil {
		return fmt.Errorf("写入自启项失败:%w", err)
	}
	return nil
}
