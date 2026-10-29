// theme.go 是界面偏好(主题/升级口径)的持久化与注入。
//
// 为什么偏好落后端、而非浏览器 localStorage:
//   界面服务每次启动监听随机端口(127.0.0.1:0),http://127.0.0.1:<port> 是一个新的源
//   (origin),localStorage 按源隔离 —— 重启后端口变了,旧主题读不回来(表现为「切换后
//   重启不保存」)。因此把偏好写到 <dataDir>/ui-prefs.json,由后端持有。
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

// prefsFile 是数据目录下的界面偏好文件名(历史名 themeFile 沿用 ui-prefs.json 落盘格式)。
const prefsFile = "ui-prefs.json"

// UIPrefs 是落盘的界面偏好。Theme ∈ {"dark","light"};IncludeUnstable = 升级接受
// 非稳定版本(预发布 beta/rc),默认 false。
type UIPrefs struct {
	Theme           string `json:"theme"`
	IncludeUnstable bool   `json:"includeUnstable"`
}

// validTheme 判定主题值合法。
func validTheme(theme string) bool { return theme == "dark" || theme == "light" }

// prefsPath 返回偏好文件的绝对路径。
func (s *Server) prefsPath() string { return filepath.Join(s.dataDir, prefsFile) }

// Prefs 读取持久化偏好(非法值回退默认:深色 + 不含预发布)。
func (s *Server) Prefs() UIPrefs { return readPrefs(s.prefsPath()) }

// savePrefs 落盘偏好(权限 0600,与 ui.json 同款:仅本机、含界面定位信息)。
// 调用方应先读取现有偏好再改单个字段写入(读-改-写),避免互覆盖字段。
func (s *Server) savePrefs(p UIPrefs) error {
	if !validTheme(p.Theme) {
		p.Theme = "dark"
	}
	blob, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return os.WriteFile(s.prefsPath(), blob, 0o600)
}

// LoadTheme 读取持久化主题;文件缺失/损坏/非法值一律回退 "dark"(与默认一致)。
func (s *Server) LoadTheme() string { return s.Prefs().Theme }

// saveTheme 读-改-写落盘主题(不影响其他偏好字段)。
func (s *Server) saveTheme(theme string) error {
	p := s.Prefs()
	p.Theme = theme
	return s.savePrefs(p)
}

// LoadTheme 包级:供宿主(初始化原生标题栏配色)在 uiserver 句柄之外读取。
func LoadTheme(dataDir string) string { return readPrefs(filepath.Join(dataDir, prefsFile)).Theme }

// readPrefs 从指定路径读取偏好,任何异常逐字段回退默认值。
func readPrefs(path string) UIPrefs {
	var p UIPrefs
	if blob, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(blob, &p); err != nil {
			return UIPrefs{Theme: "dark"}
		}
	}
	if !validTheme(p.Theme) {
		p.Theme = "dark"
	}
	return p
}

// injectTheme 把 index.html 里的 <html data-theme="dark"> 覆写为当前主题。
// 只替换第一处(即 <html> 标签),对已构建产物中的内联样式/脚本无副作用。
func injectTheme(html []byte, theme string) []byte {
	if !validTheme(theme) {
		theme = "dark"
	}
	return bytes.Replace(html, []byte(`data-theme="dark"`), []byte(`data-theme="`+theme+`"`), 1)
}
