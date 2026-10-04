package httpapi

// labels.go — 机器标签登记处 HTTP API(设置 → 服务器 的标签管理卡消费)。
//
//	GET    /api/labels          列全部登记标签(含悬置;按名排序)
//	POST   /api/labels          登记标签 {name}(写:auth + CSRF)
//	DELETE /api/labels/{name}   删登记标签(仍被机器引用 → 409 label_in_use;写:auth + CSRF)
//
// 名字校验复用构建机池字符集(runner.ValidateTerm);servers.labels 仍是机器实际标签的
// 事实来源,登记处只是「可先建后挂」的字典。

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/labels"
)

// makeListLabelsHandler 返回 GET /api/labels handler。
func makeListLabelsHandler(svc labels.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "标签服务未初始化")
			return
		}
		items, err := svc.List(r.Context())
		if err != nil {
			writeServerError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

// makeCreateLabelHandler 返回 POST /api/labels handler。
func makeCreateLabelHandler(svc labels.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "标签服务未初始化")
			return
		}
		var req struct {
			Name string `json:"name"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "请求体格式错误")
			return
		}
		l, err := svc.Create(r.Context(), req.Name)
		switch {
		case errors.Is(err, labels.ErrInvalidName):
			writeError(w, http.StatusBadRequest, "invalid_label",
				"标签名非法:字母数字开头,可含 . _ -;k=v 的键值同规则")
			return
		case errors.Is(err, labels.ErrExists):
			writeError(w, http.StatusConflict, "label_exists", "同名标签已存在")
			return
		case err != nil:
			writeServerError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, l)
	}
}

// makeDeleteLabelHandler 返回 DELETE /api/labels/{name} handler。
func makeDeleteLabelHandler(svc labels.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "标签服务未初始化")
			return
		}
		err := svc.Delete(r.Context(), chi.URLParam(r, "name"))
		switch {
		case err == nil:
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, labels.ErrNotFound):
			writeError(w, http.StatusNotFound, "label_not_found", "标签不存在")
		case errors.Is(err, labels.ErrInvalidName):
			writeError(w, http.StatusBadRequest, "invalid_label", "标签名非法")
		default:
			var inUse *labels.InUseError
			if errors.As(err, &inUse) {
				writeError(w, http.StatusConflict, "label_in_use",
					fmt.Sprintf("标签仍被 %d 台机器引用:请先从机器上移除该标签,再删除登记", inUse.Servers))
				return
			}
			writeServerError(w, err)
		}
	}
}
