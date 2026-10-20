//go:build windows

// windowstate_windows.go 持久化主窗口边界:关闭到托盘/退出时保存,启动恢复。
// walk 无内建持久化;不恢复时每次启动位置/尺寸由创建当刻内容推算,观感"漂移"。
package main

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/lxn/win"
)

// windowState 是主窗口边界的持久化形态(像素,BoundsPixels)。
type windowState struct {
	X, Y, W, H int
	Max        bool
}

func uiStatePath() string {
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		if base, err = os.UserCacheDir(); err != nil || base == "" {
			return "window.json"
		}
	}
	return filepath.Join(base, "pipewright-client", "window.json")
}

func loadWindowState() windowState {
	var st windowState
	if b, err := os.ReadFile(uiStatePath()); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

// saveWindowState 采集当前边界与最大化态落盘(错误静默 —— 持久化失败不影响使用)。
func (a *trayApp) saveWindowState() {
	if a.mw == nil {
		return
	}
	b := a.mw.BoundsPixels()
	st := windowState{X: b.X, Y: b.Y, W: b.Width, H: b.Height, Max: win.IsZoomed(a.mw.Handle())}
	if dir := filepath.Dir(uiStatePath()); dir != "." {
		_ = os.MkdirAll(dir, 0o700)
	}
	if b, err := json.Marshal(st); err == nil {
		_ = os.WriteFile(uiStatePath(), b, 0o600)
	}
}

// restoreWindowState 恢复上次边界(缺失/非法时保持声明式初始尺寸)。屏幕(分辨率/
// 显示器数)可能已变化:钳制到主屏可见区,至少留 80px 可拖拽。
func (a *trayApp) restoreWindowState(minSize Size) {
	st := loadWindowState()
	if st.W < minSize.Width || st.H < minSize.Height {
		return
	}
	sw, sh := winScreenSize()
	clamp := func(v, lo, hi int) int {
		switch {
		case hi < lo:
			return lo
		case v < lo:
			return lo
		case v > hi:
			return hi
		default:
			return v
		}
	}
	x := clamp(st.X, 80-st.W, sw-80)
	y := clamp(st.Y, 0, sh-80)
	w, h := min(st.W, sw), min(st.H, sh)
	_ = a.mw.SetBoundsPixels(walk.Rectangle{X: x, Y: y, Width: w, Height: h})
	if st.Max {
		win.ShowWindow(a.mw.Handle(), win.SW_MAXIMIZE)
	}
}

func winScreenSize() (int, int) {
	return int(win.GetSystemMetrics(win.SM_CXSCREEN)), int(win.GetSystemMetrics(win.SM_CYSCREEN))
}
