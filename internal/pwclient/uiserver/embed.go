// Package uiserver 为 pwclient 提供本地 Web UI:内嵌前端静态资源、把 /api/* 反向代理到
// 本机 IPC 服务(注入 Bearer token),并提供 IPC 服务做不到的本机动作(/local/*:
// 安装/卸载服务、开机自启、退出客户端、提权升级、打开日志)。只绑 127.0.0.1。
package uiserver

import (
	"embed"
	"io/fs"
)

// distFS 是前端构建产物(webclient/ → dist)。构建前由 Makefile 的 client-web-stub
// 生成占位 index.html,保证 go:embed 在纯后端流程下也能编译。
//
//go:embed all:dist
var distFS embed.FS

// distRoot 返回去掉 dist 前缀的资源根。
func distRoot() (fs.FS, error) {
	return fs.Sub(distFS, "dist")
}
