package uiserver

import (
	"io"
	"net/http"

	"github.com/huangchengsir/pipewright/internal/pwclient"
)

// handleAPI 把 /api/* 反向代理到本机 IPC 服务:每次请求重读 ipc.json(服务可能刚重启,
// 端口与令牌会变),注入 Bearer token;前端因此永远接触不到 IPC 令牌。
func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	cli, err := pwclient.DialIPC(s.dataDir)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "后台服务未运行(服务未安装或未启动)")
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, cli.Base+r.URL.RequestURI(), r.Body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	req.Header.Set("Authorization", "Bearer "+cli.Token)
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if accept := r.Header.Get("Accept"); accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := cli.HTTP.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "读取后台服务失败:"+err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
