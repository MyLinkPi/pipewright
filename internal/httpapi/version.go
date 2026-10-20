package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/huangchengsir/pipewright/internal/srcupdate"
	"github.com/huangchengsir/pipewright/internal/version"
)

// makeCheckUpdateHandler 处理 GET /api/version/check:查询升级源最新发布并与当前版本比对。
// 源码部署(src 非 nil)改走 git 检查(fetch + 比对上游与运行中构建所基于的提交)。检查失败
// (网络/限流)不返 5xx —— 而是 200 + UpdateInfo.CheckError,让前端稳定渲染降级态。
func makeCheckUpdateHandler(checker *version.Checker, src *srcupdate.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if src != nil {
			writeJSON(w, http.StatusOK, src.Check(r.Context()))
			return
		}
		info := checker.Check(r.Context())
		writeJSON(w, http.StatusOK, info)
	}
}

// updateResult 是 POST /api/version/update 的返回。
type updateResult struct {
	Mode    string `json:"mode"`              // binary | docker | source
	Status  string `json:"status"`            // restarting | manual | uptodate | building | busy | error
	From    string `json:"from"`              // 当前版本
	To      string `json:"to"`                // 目标版本
	Message string `json:"message"`           // 给用户的说明
	Command string `json:"command,omitempty"` // docker 模式的升级命令
}

// makeSelfUpdateHandler 处理 POST /api/version/update:执行一键自动更新。
//
//   - source 模式(源码部署):启动后台管线 git pull --ff-only → make build → install.sh,
//     立即返回 building;进度经 GET /api/version/update/status 轮询,装完自动重启(服务
//     重启 / re-exec),前端轮询 /version 等新版本就位。
//   - binary 模式(release 二进制):下载新版二进制 + 校验和核验 + 原子替换当前可执行文件,
//     回完响应后用新二进制 re-exec 自重启(同 PID 重新绑定端口,短暂不可用,前端轮询 /version 重连)。
//   - docker 模式:容器不能替换自身镜像,返回精确升级命令供用户执行。
func makeSelfUpdateHandler(checker *version.Checker, src *srcupdate.Service, inflight *updateGate, guard ReexecGuard) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 源码部署:后台管线自有序列化(同一时刻一个任务),不经 binary 的响应后 re-exec 流程。
		if src != nil {
			if err := src.StartUpdate(); err != nil {
				if errors.Is(err, srcupdate.ErrBusy) {
					writeError(w, http.StatusConflict, "update_busy", "已有升级任务在进行,请等待其完成")
					return
				}
				writeError(w, http.StatusBadRequest, "update_start_failed", err.Error())
				return
			}
			writeJSON(w, http.StatusOK, updateResult{
				Mode: "source", Status: "building",
				Message: "已启动升级:拉取代码 → 构建 → 安装,完成后自动重启。进度请留意下方状态。",
			})
			return
		}

		if !inflight.tryAcquire() {
			writeError(w, http.StatusConflict, "update_busy", "已有更新正在进行")
			return
		}
		// binary 成功路径会 re-exec(不返回),故仅在非成功路径释放;deferred 路径
		// 释放(允许更新版本的请求取消并取代等待者)。
		release := inflight.release
		var body struct {
			Force bool `json:"force"` // 有活跃任务时仍立即重启(将中断任务)
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body)

		info := checker.Check(r.Context())
		res := updateResult{From: info.Current, To: info.Latest}

		if info.CheckError != "" {
			release()
			writeError(w, http.StatusBadGateway, "update_check_failed", info.CheckError)
			return
		}
		if !info.UpdateAvailable {
			release()
			res.Status, res.Message = "uptodate", "已是最新版本"
			writeJSON(w, http.StatusOK, res)
			return
		}

		// Docker:不能自换镜像,给升级命令。
		if version.Mode() == version.ModeDocker {
			release()
			res.Mode, res.Status = "docker", "manual"
			res.Command = version.DockerUpgradeCommand()
			res.Message = "Docker 部署无法在容器内替换自身镜像。请在宿主执行升级命令(数据卷保留,新版自动迁移)。"
			writeJSON(w, http.StatusOK, res)
			return
		}

		res.Mode = "binary"
		if !version.CanSelfReplace() {
			release()
			res.Status = "manual"
			res.Message = "当前平台不支持替换运行中的程序,请手动下载新版替换。"
			writeJSON(w, http.StatusOK, res)
			return
		}

		// 下载 + 校验 + 替换二进制。失败则释放并报错(进程仍跑旧版,不影响服务)。
		if err := checker.ApplyBinaryUpdate(r.Context(), info.Latest); err != nil {
			release()
			// 标准错误信封:前端 catch 直接展示真实原因(此前 DTO 形状只显示"更新失败(500)")。
			writeError(w, http.StatusInternalServerError, "update_failed", err.Error())
			return
		}

		// 活跃任务门槛(2a):有非终态本地运行或跨环境共享构建任务且未强制时,
		// 推迟重启 —— 等待全部结束后自动 re-exec(避免孤儿构建进程:re-exec 不执行
		// 清理,新镜像启动还会把非终态运行判死,孤儿进程却在真实执行)。
		if guard != nil {
			if runs, shared := guard.ActiveTasks(); runs+shared > 0 && !body.Force {
				release()
				res.Status = "deferred"
				res.Message = fmt.Sprintf("新版已就位:当前有 %d 个本地运行、%d 个跨环境共享构建任务进行中,全部结束后自动重启生效(强制立即重启将中断它们)", runs, shared)
				writeJSON(w, http.StatusOK, res)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				log.Printf("[update] %s → %s 已就位,等待 %d 运行/%d 共享任务空闲后自动重启", info.Current, info.Latest, runs, shared)
				scheduleDeferredReexec(guard)
				return
			}
		}

		// 替换成功:先把响应发回(并 flush),再延迟 re-exec,给前端时间进入"等待重启"态。
		res.Status = "restarting"
		res.Message = "新版本已就位,正在重启…"
		writeJSON(w, http.StatusOK, res)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		log.Printf("[update] %s → %s 已就位,即将 re-exec 重启", info.Current, info.Latest)
		cancelDeferredReexec()
		go func() {
			time.Sleep(700 * time.Millisecond) // 让响应抵达客户端
			if guard != nil {
				guard.GracefulShutdown() // 同步停 mesh 等托管子进程并等退出,再 exec
			}
			if err := version.Reexec(); err != nil {
				log.Printf("[update] re-exec 失败(新版已就位,请手动重启): %v", err)
				release()
			}
		}()
	}
}

// updateGate 串行化自更新:同一时刻仅一个进行中。
type updateGate struct {
	ch chan struct{}
}

func newUpdateGate() *updateGate {
	g := &updateGate{ch: make(chan struct{}, 1)}
	g.ch <- struct{}{}
	return g
}

func (g *updateGate) tryAcquire() bool {
	select {
	case <-g.ch:
		return true
	default:
		return false
	}
}

func (g *updateGate) release() {
	select {
	case g.ch <- struct{}{}:
	default:
	}
}

// makeUpdateStatusHandler 处理 GET /api/version/update/status:源码升级后台任务的进度快照
// (步骤 / 日志 / 完成 / 失败)。非源码部署返回 supported=false,前端不启用轮询。
func makeUpdateStatusHandler(src *srcupdate.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if src == nil {
			writeJSON(w, http.StatusOK, map[string]any{"supported": false})
			return
		}
		st := src.Status()
		writeJSON(w, http.StatusOK, map[string]any{
			"supported": true,
			"running":   st.Running,
			"step":      st.Step,
			"message":   st.Message,
			"done":      st.Done,
			"error":     st.Error,
			"log":       st.Log,
		})
	}
}

// ReexecGuard 是自更新 re-exec 前的门槛与清理钩子(main 装配;nil = 无门槛直启,
// 兼容最小部署)。ActiveTasks 返回非终态本地运行数与跨环境共享构建任务数;
// GracefulShutdown 同步停掉托管子进程(mesh 监督器等)并等待退出 —— syscall.Exec
// 不执行清理,不等退出会留孤儿占端口(v1.6.4 事故)。
type ReexecGuard interface {
	ActiveTasks() (runs int, sharedJobs int)
	GracefulShutdown()
}

// deferredExec 管理等待空闲的 re-exec:同一时刻仅一个等待者;新版本的更新请求
// 会取消并取代旧等待者(等待期间轮询 10s;空闲后 GracefulShutdown → Reexec)。
var (
	deferredMu     sync.Mutex
	deferredCancel context.CancelFunc
)

func scheduleDeferredReexec(guard ReexecGuard) {
	deferredMu.Lock()
	old := deferredCancel
	ctx, cancel := context.WithCancel(context.Background())
	deferredCancel = cancel
	deferredMu.Unlock()
	if old != nil {
		old()
	}
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		defer cancel()
		for {
			if runs, shared := guard.ActiveTasks(); runs+shared == 0 {
				log.Printf("[update] 活跃任务已清空,自动 re-exec 重启")
				guard.GracefulShutdown()
				time.Sleep(700 * time.Millisecond)
				if err := version.Reexec(); err != nil {
					log.Printf("[update] 等待空闲后 re-exec 失败(新版已就位,请手动重启): %v", err)
				}
				return
			}
			select {
			case <-ctx.Done():
				return // 被新更新请求取代
			case <-t.C:
			}
		}
	}()
}

func cancelDeferredReexec() {
	deferredMu.Lock()
	c := deferredCancel
	deferredCancel = nil
	deferredMu.Unlock()
	if c != nil {
		c()
	}
}
