package uiserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/huangchengsir/pipewright/internal/pwclient"
)

// ErrUnsupported 表示当前平台不支持该本机动作(如非 Windows 的服务安装/自启/升级)。
var ErrUnsupported = errors.New("当前平台不支持该操作")

// ServiceState 是后台服务的安装/运行状态(由宿主查询平台服务管理器后回填)。
type ServiceState struct {
	Installed bool   `json:"installed"`
	State     string `json:"state"` // RUNNING | STARTING | STOPPING | STOPPED | NOT_INSTALLED | UNSUPPORTED | UNKNOWN
}

// LocalInfo 是 /local/info 的载荷:界面所需的宿主信息(不来自 IPC 服务)。
type LocalInfo struct {
	Platform          string       `json:"platform"`
	Version           string       `json:"version"`
	DataDir           string       `json:"dataDir"`
	ConfigFile        string       `json:"configFile"`
	LogFile           string       `json:"logFile"`
	Service           ServiceState `json:"service"`
	AutoStart         bool         `json:"autoStart"`
	CanInstallService bool         `json:"canInstallService"`
}

// LocalActions 由宿主(cmd/pwclient)实现:IPC 服务进程做不到的本机操作。
// 所有方法都不得阻塞超过数秒;需要等待的操作(如 UAC 安装)应立即返回由界面轮询状态。
type LocalActions interface {
	Info(ctx context.Context) (LocalInfo, error)
	// InstallService 提权安装并启动后台服务(Windows: UAC)。
	InstallService(ctx context.Context) error
	// ServiceAction 执行 start|stop|uninstall(Windows: UAC)。
	ServiceAction(ctx context.Context, action string) error
	// SetAutoStart 写入/删除登录自启项(Windows: HKCU Run)。
	SetAutoStart(ctx context.Context, enabled bool) error
	// Upgrade 提权启动独立升级进程(tag 为空 = 最新版)。
	Upgrade(ctx context.Context, tag string) error
	// Quit 退出 UI 进程(不影响后台组网服务)。
	Quit(ctx context.Context) error
	// OpenLog 用系统默认程序打开日志文件。
	OpenLog(ctx context.Context) error
	// RevealDataDir 在文件管理器中打开数据目录。
	RevealDataDir(ctx context.Context) error
}

// handleLocal 处理 /local/*:界面令牌已在路由层校验,Host 亦已校验。
func (s *Server) handleLocal(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/local/")
	switch {
	case rest == "info":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "仅支持 GET")
			return
		}
		info, err := s.local.Info(r.Context())
		if err != nil {
			writeLocalError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, info)

	case rest == "quit", rest == "open-log", rest == "reveal-data-dir":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "仅支持 POST")
			return
		}
		var err error
		switch rest {
		case "quit":
			err = s.local.Quit(r.Context())
		case "open-log":
			err = s.local.OpenLog(r.Context())
		default:
			err = s.local.RevealDataDir(r.Context())
		}
		if err != nil {
			writeLocalError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	case rest == "autostart":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "仅支持 POST")
			return
		}
		var body struct {
			Enabled bool `json:"enabled"`
		}
		if err := decodeJSON(w, r, &body); err != nil {
			return
		}
		if err := s.local.SetAutoStart(r.Context(), body.Enabled); err != nil {
			writeLocalError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	case rest == "upgrade":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "仅支持 POST")
			return
		}
		var body struct {
			Tag string `json:"tag"`
		}
		if err := decodeJSON(w, r, &body); err != nil {
			return
		}
		tag := strings.TrimSpace(body.Tag)
		// tag 会被宿主拼入提权命令行与下载 URL:白名单校验(空 = 最新版)。
		if err := pwclient.ValidateTag(tag); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.local.Upgrade(r.Context(), tag); err != nil {
			writeLocalError(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})

	case strings.HasPrefix(rest, "service/"):
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "仅支持 POST")
			return
		}
		action := strings.TrimPrefix(rest, "service/")
		switch action {
		case "install":
			if err := s.local.InstallService(r.Context()); err != nil {
				writeLocalError(w, err)
				return
			}
		case "start", "stop", "uninstall":
			if err := s.local.ServiceAction(r.Context(), action); err != nil {
				writeLocalError(w, err)
				return
			}
		default:
			writeError(w, http.StatusBadRequest, "未知服务动作:"+action)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})

	default:
		writeError(w, http.StatusNotFound, "未知本地端点")
	}
}

// writeLocalError 把本机动作错误映射为 HTTP 状态:不支持 → 501,其余 → 500。
func writeLocalError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrUnsupported) {
		writeError(w, http.StatusNotImplemented, err.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}

// decodeJSON 解析请求体(上限 64KB);失败时已写错误响应。
func decodeJSON(w http.ResponseWriter, r *http.Request, out any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(out); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return err
	}
	return nil
}
