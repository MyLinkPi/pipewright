// ui.go 是三平台通用的界面装配:按平台决定是否在本进程跑核心(Windows 由后台服务提供,
// Linux/macOS 前台运行),然后启动 uiserver 并交给平台桌面形态(Windows 托盘 + WebView2,
// 其他平台打开系统浏览器)。
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/huangchengsir/pipewright/internal/pwclient/uiserver"
)

// runTray 是默认命令:以界面形态启动(Windows 托盘 + 主窗口,其他平台浏览器)。
func runTray() { uiMain(startMinimized, "") }

// uiMain 启动界面形态。noOpen = 不自动弹窗/浏览器(供 e2e 与开发联调,仅打印 URL)。
func uiMain(noOpen bool, listen string) {
	// 单实例在启动 uiserver 之前判定:否则后起进程会覆盖 <dataDir>/ui.json(已有实例的
	// 界面定位文件),退出时还会把它删掉。
	if !preflightDesktop() {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var c *core
	if mode := localCoreMode(); mode != "" {
		var err error
		c, err = runCore(mode)
		if err != nil {
			fmt.Fprintln(os.Stderr, "启动失败:", err)
			os.Exit(1)
		}
	}

	srv, err := uiserver.Start(ctx, uiserver.Options{
		DataDir: dataDir(),
		Local:   newLocalActions(c),
		Listen:  listen,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "启动界面服务失败:", err)
		os.Exit(1)
	}
	if noOpen {
		// 标准输出打印带令牌的地址:e2e/开发联调用它定位界面。
		fmt.Println(srv.URL())
	}

	runDesktop(srv.URL(), noOpen)

	if c != nil {
		// 与服务停止分支同款契约:等核心彻底退出(对账循环收尾强杀并回收全部托管
		// EasyTier 子进程)后再退出进程 —— cancel 只触发信号,杀进程/收尸跑在本进程
		// goroutine 上,主进程立即退出会遗留孤儿 core 占用 TUN 与 RPC 端口。
		c.cancel()
		select {
		case <-c.stopWait:
		case <-time.After(15 * time.Second):
			log.Println("[pwclient] 等待核心退出超时,强制退出")
		}
	}
}

// runUICmd 解析 `pwclient ui [--no-open] [--ui-listen addr]`。
func runUICmd(args []string) {
	noOpen := startMinimized
	listen := ""
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--no-open" || arg == "-no-open":
			noOpen = true
		case arg == "--ui-listen" || arg == "-ui-listen":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--ui-listen 缺少地址")
				os.Exit(2)
			}
			listen = args[i+1]
			i++
		case strings.HasPrefix(arg, "--ui-listen="):
			listen = strings.TrimPrefix(arg, "--ui-listen=")
		default:
			fmt.Fprintf(os.Stderr, "未知参数:%s\n\n", arg)
			usage()
			os.Exit(2)
		}
	}
	uiMain(noOpen, listen)
}
