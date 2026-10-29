// theme.go 是界面主题(外观)的持久化与注入。
//
// 为什么主题落后端、而非浏览器 localStorage:
//   界面服务每次启动监听随机端口(127.0.0.1:0),http://127.0.0.1:<port> 是一个新的源
//   (origin),localStorage 按源隔离 —— 重启后端口变了,旧主题读不回来(表现为「切换后
//   重启不保存」)。因此把主题写到 <dataDir>/ui-prefs.json,由后端持有。
//
// 首屏正确性:handleStatic 在返回 index.html 时把持久化主题注入 <html data-theme>,
// 使 JS 执行前就是正确主题,既避免闪烁,也不依赖会失效的 localStorage。
package uiserver

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
)

// themeFile 是数据目录下的界面偏好文件名。
const themeFile = "ui-prefs.json"

// uiPrefs 是落盘的界面偏好。Theme ∈ {"dark","light"}。
type uiPrefs struct {
	Theme string `json:"theme"`
}

// validTheme 判定主题值合法。
func validTheme(theme string) bool { return theme == "dark" || theme == "light" }

// prefsPath 返回偏好文件的绝对路径。
func (s *Server) prefsPath() string { return filepath.Join(s.dataDir, themeFile) }

// LoadTheme 读取持久化主题;文件缺失/损坏/非法值一律回退 "dark"(与默认一致)。
func (s *Server) LoadTheme() string { return readTheme(s.prefsPath()) }

// saveTheme 落盘主题(权限 0600,与 ui.json 同款:仅本机、含界面定位信息)。
func (s *Server) saveTheme(theme string) error {
	blob, err := json.Marshal(uiPrefs{Theme: theme})
	if err != nil {
		return err
	}
	return os.WriteFile(s.prefsPath(), blob, 0o600)
}

// LoadTheme 包级:供宿主(初始化原生标题栏配色)在 uiserver 句柄之外读取。
func LoadTheme(dataDir string) string { return readTheme(filepath.Join(dataDir, themeFile)) }

// readTheme 从指定路径读取主题,任何异常回退 "dark"。
func readTheme(path string) string {
	blob, err := os.ReadFile(path)
	if err != nil {
		return "dark"
	}
	var p uiPrefs
	if err := json.Unmarshal(blob, &p); err != nil || !validTheme(p.Theme) {
		return "dark"
	}
	return p.Theme
}

// injectTheme 把 index.html 里的 <html data-theme="dark"> 覆写为当前主题。
// 只替换第一处(即 <html> 标签),对已构建产物中的内联样式/脚本无副作用。
func injectTheme(html []byte, theme string) []byte {
	if !validTheme(theme) {
		theme = "dark"
	}
	return bytes.Replace(html, []byte(`data-theme="dark"`), []byte(`data-theme="`+theme+`"`), 1)
}
