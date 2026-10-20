//go:build !windows

// console_other.go 是非 Windows 平台的控制台辅助占位(客户端首要支持 Win11)。
package main

// attachParentConsole 在非 Windows 平台无事可做。
func attachParentConsole() {}
