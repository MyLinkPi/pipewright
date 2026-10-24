package secscan

// 雷池 SafeLine WAF 的接入面:kind=safeline 的 scanner 实例 = 管理面地址(host:port,
// 管理台自签 HTTPS)+ API Token(控制台生成,vault 密封)。本文件提供:
//   - WafClient:Ping(探活/鉴权)/ Stats(拦截统计)/ UpsertCert(证书原位同步);
//   - Service.WafOverview:安全中心「防护态势」的数据底座;
//   - Service.WafGateway:certdeploy 推证书的适配(resend 证书续期自动联动)。
//
// OpenAPI 事实(以部署实例 /api/open 文档为准,7.2.3+ 控制台可查):认证头
// X-SLCE-API-TOKEN;证书 GET/POST /api/open/cert、PUT /api/open/cert/{id}
// (body {type:"manual", manual:{crt,key}});统计 GET /api/open/statistics(响应
// 结构存在版本差异,宽容提取;失败回退 /api/open/event 的分页 total)。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// wafHTTPTimeout 是单次 SafeLine API 调用预算(探活/统计/推证书)。
const wafHTTPTimeout = 10 * time.Second

// WafEndpoint 是一个 SafeLine 管理面接入点(BaseURL 含 scheme,自签证书免校验)。
type WafEndpoint struct {
	BaseURL string // https://host:port
	Token   string // API Token(进程内即取即用,绝不落日志)
}

// WafStats 是一个实例的拦截态势(口径:SafeLine 统计接口的宽容提取)。
type WafStats struct {
	BlockedToday int `json:"blockedToday"`
	Blocked7d    int `json:"blocked7d"`
}

// WafClient 是 SafeLine OpenAPI 的最小面(测试注入 fake;生产 httpWafClient)。
type WafClient interface {
	Ping(ctx context.Context, ep WafEndpoint) *TestResult
	Stats(ctx context.Context, ep WafEndpoint) (*WafStats, error)
	UpsertCert(ctx context.Context, ep WafEndpoint, name, crtPEM, keyPEM string) error
}

// httpWafClient 是 WafClient 的 HTTP 实现。管理台为自签 HTTPS:内网接入场景与
// probeTrivyServer 同姿态(InsecureSkipVerify);Token 只进请求头。
type httpWafClient struct{}

// wafDo 发一次 JSON 请求;200-299 → body;其余 → 人话错误(含响应尾部,无 Token)。
func (httpWafClient) wafDo(ctx context.Context, ep WafEndpoint, method, path string, payload any) ([]byte, error) {
	url := strings.TrimRight(ep.BaseURL, "/") + path
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = strings.NewReader(string(raw))
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-SLCE-API-TOKEN", ep.Token)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{
		Timeout:   wafHTTPTimeout,
		Transport: &http.Transport{TLSClientConfig: insecureTLS()},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		tail := tailClip(string(raw), 200)
		if tail == "" {
			tail = resp.Status
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("鉴权失败(HTTP %d):API Token 不匹配或未生效", resp.StatusCode)
		}
		return nil, fmt.Errorf("HTTP %d:%s", resp.StatusCode, tail)
	}
	return raw, nil
}

// Ping 探活:GET 证书列表(轻量只读)——区分「不可达 / Token 错误 / 正常」。
func (c httpWafClient) Ping(ctx context.Context, ep WafEndpoint) *TestResult {
	start := time.Now()
	_, err := c.wafDo(ctx, ep, http.MethodGet, "/api/open/cert", nil)
	if err != nil {
		return &TestResult{Err: err, Detail: "SafeLine 探活失败:" + humanProbeError(err)}
	}
	return &TestResult{OK: true, LatencyMs: time.Since(start).Milliseconds(), Detail: "SafeLine 管理面可达,API Token 有效"}
}

// wafStatsPayload 是统计接口响应的宽容投影(data 平铺或嵌套都取得到)。
type wafStatsPayload struct {
	Total     *int `json:"total"`
	Blocked   *int `json:"blocked"`
	Blocked7d *int `json:"blocked_7d"`
	Blocked7D *int `json:"blocked7d"`
	Today     *int `json:"blocked_today"`
	TodayAlt  *int `json:"blockedToday"`
	Data      *struct {
		Total    *int `json:"total"`
		Blocked  *int `json:"blocked"`
		Today    *int `json:"blocked_today"`
		TodayAlt *int `json:"blockedToday"`
	} `json:"data"`
}

// Stats 拉拦截统计:优先 /api/open/statistics(宽容字段提取);失败回退
// /api/open/event 分页 total 当作累计值。两路都不成 → 人话错误(态势卡显示
// 「统计暂不可用」,不假装为 0)。
func (c httpWafClient) Stats(ctx context.Context, ep WafEndpoint) (*WafStats, error) {
	raw, err := c.wafDo(ctx, ep, http.MethodGet, "/api/open/statistics", nil)
	if err == nil {
		var p wafStatsPayload
		if json.Unmarshal(raw, &p) == nil {
			pick := func(v ...*int) int {
				for _, x := range v {
					if x != nil {
						return *x
					}
				}
				return 0
			}
			today := pick(p.Today, p.TodayAlt, p.Data.Today, p.Data.TodayAlt)
			week := pick(p.Blocked7d, p.Blocked7D, p.Total, p.Blocked, p.Data.Total, p.Data.Blocked)
			if today > week {
				week = today
			}
			return &WafStats{BlockedToday: today, Blocked7d: week}, nil
		}
	}
	// 回退:事件分页 total(累计口径)。
	if raw2, e2 := c.wafDo(ctx, ep, http.MethodGet, "/api/open/event?page=1&page_size=1", nil); e2 == nil {
		var ev struct {
			Total *int `json:"total"`
			Data  *struct {
				Total *int `json:"total"`
			} `json:"data"`
		}
		if json.Unmarshal(raw2, &ev) == nil {
			n := 0
			if ev.Total != nil {
				n = *ev.Total
			} else if ev.Data != nil && ev.Data.Total != nil {
				n = *ev.Data.Total
			}
			return &WafStats{BlockedToday: n, Blocked7d: n}, nil
		}
	}
	if err != nil {
		return nil, fmt.Errorf("统计接口不可用:%v", err)
	}
	return nil, fmt.Errorf("统计响应无法解析(可能与版本相关,请在实例控制台查看)")
}

// safelineCertRow 是证书列表项的最小投影(id + 宽容的名称字段)。
type safelineCertRow struct {
	ID   any    `json:"id"`
	Name string `json:"name"`
}

// UpsertCert 同步一张证书:列表里按名称找到 → PUT /api/open/cert/{id} 原位更新
// (站点绑定关系不变,续期即生效);找不到 → POST 新建。创建 body 附 name 字段
// (旧版忽略该字段无害)。
func (c httpWafClient) UpsertCert(ctx context.Context, ep WafEndpoint, name, crtPEM, keyPEM string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("证书名不能为空")
	}
	listRaw, err := c.wafDo(ctx, ep, http.MethodGet, "/api/open/cert", nil)
	id := ""
	if err == nil {
		var list struct {
			Certs []safelineCertRow `json:"certs"`
			Data  []safelineCertRow `json:"data"`
		}
		if json.Unmarshal(listRaw, &list) == nil {
			rows := list.Certs
			if len(rows) == 0 {
				rows = list.Data
			}
			for _, row := range rows {
				if row.Name == name {
					id = fmt.Sprint(row.ID)
					break
				}
			}
		}
	}
	body := map[string]any{
		"type": "manual",
		"name": name,
		"manual": map[string]string{
			"crt": crtPEM,
			"key": keyPEM,
		},
	}
	if id != "" {
		if _, err := c.wafDo(ctx, ep, http.MethodPut, "/api/open/cert/"+id, body); err != nil {
			return fmt.Errorf("更新证书失败:%v", err)
		}
		return nil
	}
	if _, err := c.wafDo(ctx, ep, http.MethodPost, "/api/open/cert", body); err != nil {
		// 列表失败但仍创建失败:统一人话。
		return fmt.Errorf("上传证书失败:%v", err)
	}
	return nil
}

// ---------- Service 侧 ----------

// WafInstanceStatus 是防护态势的单实例视图。
type WafInstanceStatus struct {
	InstanceID      string    `json:"instanceId"`
	Name            string    `json:"name"`
	Endpoint        string    `json:"endpoint"` // host:port(不含 scheme,展示用)
	TokenConfigured bool      `json:"tokenConfigured"`
	OK              bool      `json:"ok"`
	Detail          string    `json:"detail,omitempty"`
	LatencyMs       int64     `json:"latencyMs,omitempty"`
	Stats           *WafStats `json:"stats,omitempty"`
}

// WafOverview 汇总全部 safeline 实例的连通与拦截态势(顺序探测,单实例独立短超时;
// 任一实例失败不影响其余)。无实例 → 空列表(前端隐藏卡片)。
func (s *service) WafOverview(ctx context.Context) ([]WafInstanceStatus, error) {
	list, err := s.ListScanners(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]WafInstanceStatus, 0)
	for _, inst := range list {
		if inst.Kind != KindSafeline {
			continue
		}
		st := WafInstanceStatus{
			InstanceID: inst.ID, Name: inst.Name,
			Endpoint:        fmt.Sprintf("%s:%d", inst.Host, inst.Port),
			TokenConfigured: inst.TokenConfigured,
		}
		ep, perr := s.wafEndpoint(ctx, inst.ID)
		if perr != nil || !inst.TokenConfigured {
			st.Detail = "待配置 API Token:在 SafeLine 控制台生成后,于本页扫描器实例中补填"
			out = append(out, st)
			continue
		}
		if ping := s.waf.Ping(ctx, ep); !ping.OK {
			st.Detail = ping.Detail
			out = append(out, st)
			continue
		} else {
			st.OK, st.LatencyMs = true, ping.LatencyMs
		}
		cctx, cancel := context.WithTimeout(ctx, wafHTTPTimeout)
		if stats, serr := s.waf.Stats(cctx, ep); serr == nil {
			st.Stats = stats
		} else {
			st.Detail = "在线,但统计暂不可用:" + truncateRunes(serr.Error(), 120)
		}
		cancel()
		out = append(out, st)
	}
	return out, nil
}

// wafEndpoint 从实例解出接入点(含 Token 解封)。未配置 Token → 错误。
func (s *service) wafEndpoint(ctx context.Context, instanceID string) (WafEndpoint, error) {
	inst, err := s.GetScanner(ctx, instanceID)
	if err != nil {
		return WafEndpoint{}, err
	}
	var sealed []byte
	if err := s.db.QueryRowContext(ctx,
		`SELECT token_sealed FROM scanner_instances WHERE id = ?`, inst.ID).Scan(&sealed); err != nil {
		return WafEndpoint{}, err
	}
	token, err := s.openToken(sealed)
	if err != nil {
		return WafEndpoint{}, err
	}
	if token == "" {
		return WafEndpoint{}, fmt.Errorf("实例未配置 API Token")
	}
	return WafEndpoint{
		BaseURL: fmt.Sprintf("https://%s:%d", inst.Host, inst.Port),
		Token:   token,
	}, nil
}

// UpsertCert 是 certdeploy.WafGateway 的推证书实现:按安全中心实例解析接入点并原位
// 同步证书(见 main.go 装配)。
func (s *service) UpsertCert(ctx context.Context, instanceID, name, crtPEM, keyPEM string) error {
	ep, err := s.wafEndpoint(ctx, instanceID)
	if err != nil {
		return fmt.Errorf("WAF 实例不可用:%v", err)
	}
	return s.waf.UpsertCert(ctx, ep, name, crtPEM, keyPEM)
}

// WafInstanceServerID 返回实例登记的部署宿主 server_id(certdeploy 归属用;可为空)。
func (s *service) WafInstanceServerID(ctx context.Context, instanceID string) (string, error) {
	inst, err := s.GetScanner(ctx, instanceID)
	if err != nil {
		return "", err
	}
	if inst.Kind != KindSafeline {
		return "", fmt.Errorf("实例 %s 不是 SafeLine WAF", inst.Name)
	}
	return inst.ServerID, nil
}
