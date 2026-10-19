// artifact_transfer.go 是产物管理面「传输到其他环境」的 HTTP 面(admin 组挂载):
//   - POST   /api/runs/{id}/artifacts/transfer     :发起到对端环境的异步传输任务;
//   - GET    /api/artifacts/transfers/{transferId} :任务状态轮询(前端 2s 节拍);
//   - DELETE /api/artifacts/transfers/{transferId} :取消进行中的传输(镜像导出经 ctx
//     取消终止;已终态 → 409)。
//
// 发送核心与 mesh_push 流水线节点同一代码路径(build.pushArtifactsToPeer);对端经既有
// /api/mesh/push 端点接收(blob 内容寻址去重、镜像导入内置 registry、按 origin 自动建
// 「被推送源」项目落修订),接收侧零改动;对端 ACL(push_allowed)把门,未授权 → 403
// 语义的人读错误文本。任务表进程内存态:服务重启后状态查询 404(可重新发起,重传只补缺)。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/build"
	"github.com/huangchengsir/pipewright/internal/mesh"
	"github.com/huangchengsir/pipewright/internal/run"
)

// artifactTransferService 抽象产物传输服务(*build.ArtifactTransferService 原样满足;
// 测试注入 fake)。
type artifactTransferService interface {
	Start(ctx context.Context, in build.TransferInput) (string, error)
	Get(id string) (*build.TransferStatus, bool)
	Cancel(id string) bool
}

// makeArtifactTransferStartHandler 返回 POST /api/runs/{id}/artifacts/transfer(admin):
// body { nodeId, artifactIds[], revisionLabel? } → { transferId }。校验失败同步报错
// (400/404/409/429/503);传输执行异步,成败经状态端点轮询,终态留审计(装配层 OnSettled)。
func makeArtifactTransferStartHandler(svc artifactTransferService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "产物传输服务未初始化")
			return
		}
		var body struct {
			NodeID        string   `json:"nodeId"`
			ArtifactIDs   []string `json:"artifactIds"`
			RevisionLabel string   `json:"revisionLabel"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_body", "请求体不是合法 JSON")
			return
		}
		if strings.TrimSpace(body.NodeID) == "" || len(body.ArtifactIDs) == 0 {
			writeError(w, http.StatusBadRequest, "invalid_body", "缺少目标节点或产物清单")
			return
		}
		id, err := svc.Start(r.Context(), build.TransferInput{
			RunID:         chi.URLParam(r, "id"),
			PeerNodeID:    body.NodeID,
			ArtifactIDs:   body.ArtifactIDs,
			RevisionLabel: body.RevisionLabel,
		})
		if err != nil {
			switch {
			case errors.Is(err, run.ErrNotFound):
				writeError(w, http.StatusNotFound, "run_not_found", "运行不存在")
			case errors.Is(err, run.ErrArtifactNotFound):
				writeError(w, http.StatusNotFound, "artifact_not_found", "产物不存在")
			case errors.Is(err, mesh.ErrNotFound):
				writeError(w, http.StatusNotFound, "peer_not_found", "目标节点不存在(节点表未登记)")
			case errors.Is(err, build.ErrArtifactNotTransferable):
				writeError(w, http.StatusBadRequest, "artifact_not_transferable", err.Error())
			case errors.Is(err, build.ErrInvalidTransferInput):
				writeError(w, http.StatusBadRequest, "invalid_body", "传输请求参数非法")
			case errors.Is(err, build.ErrTransferPeerNotNetworked):
				writeError(w, http.StatusConflict, "peer_not_networked", "目标节点无虚拟 IP(EasyTier 未组网)")
			case errors.Is(err, build.ErrTransferBusy):
				writeError(w, http.StatusTooManyRequests, "transfer_busy", "传输任务并发已达上限,请稍后再试")
			case errors.Is(err, build.ErrTransferUnavailable):
				writeError(w, http.StatusServiceUnavailable, "transfer_unavailable", "传输服务依赖未装配(集群/镜像导出器/制品库)")
			default:
				writeServerError(w, err)
			}
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"transferId": id})
	}
}

// makeArtifactTransferStatusHandler 返回 GET /api/artifacts/transfers/{transferId}:
// 任务状态快照(running/success/failed/canceled + 阶段文本/结果/错误)。未知 id → 404
// (服务重启丢会话的语义,前端可提示重新发起)。
func makeArtifactTransferStatusHandler(svc artifactTransferService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "产物传输服务未初始化")
			return
		}
		st, ok := svc.Get(chi.URLParam(r, "transferId"))
		if !ok {
			writeError(w, http.StatusNotFound, "transfer_not_found", "传输任务不存在(服务重启后会丢失会话,可重新发起)")
			return
		}
		writeJSON(w, http.StatusOK, st)
	}
}

// makeArtifactTransferCancelHandler 返回 DELETE /api/artifacts/transfers/{transferId}:
// 取消进行中的传输;未知 → 404,已终态 → 409。
func makeArtifactTransferCancelHandler(svc artifactTransferService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "产物传输服务未初始化")
			return
		}
		if st, ok := svc.Get(chi.URLParam(r, "transferId")); ok && st.Status != build.TransferRunning {
			writeError(w, http.StatusConflict, "transfer_not_running", "传输已结束,无需取消")
			return
		}
		if !svc.Cancel(chi.URLParam(r, "transferId")) {
			writeError(w, http.StatusNotFound, "transfer_not_found", "传输任务不存在")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}
