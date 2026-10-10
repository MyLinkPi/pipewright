package deploy

// health_probe.go 实现「health_check 流水线节点」的目标机健康探测(此前该节点是空转占位):
// 经 selector/serverId 圈选目标机(与部署节点同一套选择语义,含 selectorMode 且/或),
// 用 deploy.HealthCheck 在目标机上探测(http=curl / command=自定义命令,带重试);
// 无目标机且为 http 模式 → 从平台本机直接探测(不经 SSH)。

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// HealthProbeResult 是一台目标机(或平台本机)的健康探测结果。
type HealthProbeResult struct {
	ServerID   string // 平台本机探测时为空
	ServerName string // 平台本机探测时为 "platform"
	OK         bool
	Message    string
}

// CheckHealth 对 selector 圈选的目标机逐台做健康探测。返回逐机结果;全部通过 → nil error,
// 任一未通过 → 返回结果 + 人读 error(调用方据此令节点失败)。选择器语法非法 → ErrInvalidSelector。
// 目标为空(选择器空 / 零命中)且 hc 为 http 型 → 从平台本机探测;command 型 → 明确报错
// (命令探测必须落在目标机上,本机执行语义不明)。
func (s *service) CheckHealth(ctx context.Context, selector, selectorMode string, hc *HealthCheck) ([]HealthProbeResult, error) {
	if !hc.enabled() {
		return nil, fmt.Errorf("健康检查节点未配置探测(url 或 command)")
	}
	servers, err := s.resolveTargets(ctx, nil, selector, normalizeSelectorMode(selectorMode))
	if err != nil {
		return nil, err
	}
	if len(servers) == 0 {
		if hc.Type == HealthCheckHTTP {
			res := probeLocalHTTP(ctx, hc)
			if !res.OK {
				return []HealthProbeResult{res}, fmt.Errorf("%s", res.Message)
			}
			return []HealthProbeResult{res}, nil
		}
		return nil, fmt.Errorf("健康检查节点未圈选任何目标机:command 探测须指定目标机(selector 或 serverId)")
	}
	// 目标机可见性:探测前明示选择器圈中了哪些机器(与部署路径同格式)。
	cmdLogFrom(ctx)(cmdStreamStdout, "", fmt.Sprintf("→ 目标机(%d 台):%s", len(servers), strings.Join(serverDisplayNames(servers), ", ")))

	out := make([]HealthProbeResult, 0, len(servers))
	var failedMsgs []string
	for _, srv := range servers {
		// 单机作用域:探测命令输出归属到该机(步骤 × 机器分组)。
		perr := s.runHealthCheck(scopeCmdLog(ctx, srv.Name), srv.ID, hc)
		r := HealthProbeResult{ServerID: srv.ID, ServerName: srv.Name}
		if perr == nil {
			r.OK = true
			r.Message = "健康检查通过"
		} else {
			r.Message = perr.Error()
			failedMsgs = append(failedMsgs, srv.Name+": "+perr.Error())
		}
		out = append(out, r)
	}
	if len(failedMsgs) > 0 {
		return out, fmt.Errorf("健康检查失败:%s", strings.Join(failedMsgs, ";"))
	}
	return out, nil
}

// probeLocalHTTP 从平台本机直接做 HTTP 探测(不经 SSH;健康检查节点未圈选目标机时的兜底)。
// 探测不发 SSH 命令(Go http 客户端直连),但每次尝试仍按「执行的命令」口径回显等效 curl
// (与目标机路径 runHealthCheck 的命令回显同形,让运行日志说清「用什么检查的」),
// 失败时同步回显原因。
func probeLocalHTTP(ctx context.Context, hc *HealthCheck) HealthProbeResult {
	res := HealthProbeResult{ServerName: "platform"}
	client := &http.Client{Timeout: hc.timeout()}
	url := strings.TrimSpace(hc.URL)
	lg := cmdLogFrom(ctx)
	var lastErr error
	attempts := hc.retries()
	for i := 0; i < attempts; i++ {
		if err := ctx.Err(); err != nil {
			res.Message = "健康检查中止:" + err.Error()
			return res
		}
		// 等效命令回显:本机探测的语义即 `curl -fsS --max-time <T> <url>`(curl -f:4xx/5xx 即失败)。
		if url != "" {
			lg(cmdStreamStdout, "", fmt.Sprintf("$ curl -fsS --max-time %d %s  (平台本机 HTTP 探测,不经 SSH)", int(hc.timeout().Seconds()), url))
		}
		req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if rerr != nil {
			res.Message = "健康检查 URL 非法:" + rerr.Error()
			lg(cmdStreamStderr, "", "  ✗ "+res.Message)
			return res
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			lg(cmdStreamStderr, "", "  ✗ "+truncate(err.Error()))
		} else {
			_ = resp.Body.Close()
			// curl -f 语义:2xx 即健康;4xx/5xx 视为失败。
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				res.OK = true
				res.Message = fmt.Sprintf("健康检查通过(HTTP %d)", resp.StatusCode)
				return res
			}
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			lg(cmdStreamStderr, "", fmt.Sprintf("  ✗ HTTP %d", resp.StatusCode))
		}
		// 还有后续尝试才等间隔(与 runHealthCheck 同语义:末次失败立即返回,不空等一个间隔)。
		if i == attempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			res.Message = "健康检查中止:" + ctx.Err().Error()
			return res
		case <-time.After(hc.interval()):
		}
	}
	res.Message = "健康检查失败(已重试 " + strconv.Itoa(attempts) + " 次):" + fmt.Sprint(lastErr)
	return res
}
