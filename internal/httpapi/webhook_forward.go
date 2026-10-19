// webhook_forward.go 是 webhook 投递的集群转发(入口侧)。
//
// 场景:Git 平台只够得到集群中某一台机器,而 webhook token 属于另一环境。任一节点收到
// POST /api/webhooks/{token} 且本地无此 token 时,把原始投递(头白名单 + body)经 mesh
// 签名通道转发给各对端;token 所属环境(唯一持有密钥,能通过验签)处理并把响应回传,
// 入口节点原样透传给 Git 平台。归属判定完全在持有触发配置的对端本地完成,转发节点
// 不需要(也拿不到)任何密钥;对端按 trigger_allowed 授权单决定是否接受(默认拒绝)。
//
// 防环:对端经 /api/mesh/webhooks/{token} 直达 receiver,不经过公网 handler,不会再转发;
// 也不做 A→B→C 链式转发(节点注册表经发现循环全集群一致,入口节点总能直达归属环境)。
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/huangchengsir/pipewright/internal/mesh"
)

// webhookForwardTimeout 是一次转发的整体时限(对端理论可达,失联快速失败;
// 需在常见 Git 平台 ~10s 的 webhook 超时内给调用方留出响应时间)。
const webhookForwardTimeout = 8 * time.Second

// webhookOwnerTTL 是 token→归属环境 缓存项的存活期(归属实际不会漂移,TTL 只为
// 兜底对端重建/项目删除等罕见变化;命中可跳过 fan-out 直达归属环境)。
const webhookOwnerTTL = 10 * time.Minute

// forwardOutcome 是一次转发尝试的分类结果。
type forwardOutcome int

const (
	// forwardMiss:集群未启用/无对端,或所有可达对端均无此 token(维持本地 404)。
	forwardMiss forwardOutcome = iota
	// forwardHit:取得归属环境的确定性响应(status+body 待透传)。
	forwardHit
	// forwardUnreachable:有候选对端但全部传输失败(502,诚实暴露集群失联)。
	forwardUnreachable
)

// WebhookForwarder 把公网收到的 webhook 投递转发给 token 所属的对端环境。
// 零值不可用,经 NewWebhookForwarder 构造;mesh 未启用时自动降级为不转发。
type WebhookForwarder struct {
	mesh    mesh.Service
	client  *mesh.Client
	timeout time.Duration

	mu     sync.Mutex
	owners map[string]webhookOwnerEntry // token → 归属环境(仅在 accepted 后缓存)
}

// webhookOwnerEntry 是 token→归属环境缓存项。
type webhookOwnerEntry struct {
	nodeID    string
	expiresAt time.Time
}

// NewWebhookForwarder 构造转发器(svc 提供本节点身份与对端注册表;client 发签名请求)。
func NewWebhookForwarder(svc mesh.Service, client *mesh.Client) *WebhookForwarder {
	return &WebhookForwarder{
		mesh:    svc,
		client:  client,
		timeout: webhookForwardTimeout,
		owners:  make(map[string]webhookOwnerEntry),
	}
}

// forwardReply 是单个对端的一次转发应答。
type forwardReply struct {
	peer   mesh.Peer
	status int
	body   []byte
	// definitive:对端给出了确定语义的响应(200/401/400 等)。404(无此 token)与
	// 403(未获授权)不算:403 与 token 无关(默认全拒绝,任何对端对任何 token 都 403),
	// 无法与「非归属环境」区分,只能忽略。
	definitive bool
	// accepted:对端 200 且 accepted:true(归属环境命中并创建了运行)。
	accepted bool
	// transportErr:请求未送达(拨号失败/超时/取消)。
	transportErr bool
}

// relayWebhookToMesh 尝试转发并(命中/不可达时)写响应;返回是否已写响应。
// miss → 返回 false,由调用方维持本地 404。
func relayWebhookToMesh(w http.ResponseWriter, fwd *WebhookForwarder, r *http.Request, token string, body []byte) bool {
	if fwd == nil {
		return false
	}
	outcome, status, respBody, peerName := fwd.Forward(r.Context(), token, r.Header, body)
	switch outcome {
	case forwardHit:
		// 注记归属环境便于排查(响应体是对端原样 JSON,Git 平台忽略额外头)。
		if peerName != "" {
			w.Header().Set("X-PW-Webhook-Forwarded-For", peerName)
		}
		w.WriteHeader(status)
		_, _ = w.Write(respBody)
		return true
	case forwardUnreachable:
		writeError(w, http.StatusBadGateway, "webhook_forward_unreachable", "无法到达集群节点")
		return true
	default:
		return false
	}
}

// Forward 把一次投递转发给 token 所属环境,返回结果分类与待透传的响应。
// ctx 应为原始请求的 context(内部再叠加整体超时)。
func (f *WebhookForwarder) Forward(ctx context.Context, token string, header http.Header, body []byte) (forwardOutcome, int, []byte, string) {
	if f == nil || f.mesh == nil || f.client == nil {
		return forwardMiss, 0, nil, ""
	}
	st, err := f.mesh.Settings(ctx)
	if err != nil || !st.Enabled {
		return forwardMiss, 0, nil, ""
	}
	peers, err := f.mesh.ListPeers(ctx)
	if err != nil {
		return forwardMiss, 0, nil, ""
	}
	candidates := make([]mesh.Peer, 0, len(peers))
	for _, p := range peers {
		if strings.TrimSpace(p.VirtualIP) == "" || p.NodeID == st.NodeID {
			continue
		}
		candidates = append(candidates, p)
	}
	if len(candidates) == 0 {
		return forwardMiss, 0, nil, ""
	}

	// 缓存命中 → 直达归属环境(失联/归属失效则回退全量 fan-out)。
	if nodeID, ok := f.lookupOwner(token); ok {
		for _, p := range candidates {
			if p.NodeID != nodeID {
				continue
			}
			status, respBody, ferr := f.client.ForwardWebhook(ctx, &p, token, header, body)
			if ferr == nil && status != http.StatusNotFound {
				f.renewOwner(token, nodeID)
				return forwardHit, status, respBody, p.Name
			}
			f.invalidateOwner(token)
			break
		}
	}

	fctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()

	replies := make(chan forwardReply, len(candidates))
	for _, p := range candidates {
		go func(p mesh.Peer) {
			status, respBody, ferr := f.client.ForwardWebhook(fctx, &p, token, header, body)
			rep := forwardReply{peer: p, status: status, body: respBody}
			if ferr != nil {
				rep.transportErr = true
			} else {
				rep.definitive = status != http.StatusNotFound && status != http.StatusForbidden
				if status == http.StatusOK {
					var probe struct {
						Accepted bool `json:"accepted"`
					}
					_ = json.Unmarshal(respBody, &probe)
					rep.accepted = probe.Accepted
				}
			}
			replies <- rep
		}(p)
	}

	// 收齐全部应答;拿到确定性响应后取消其余在途请求(避免多环境重复处理;
	// token 全局唯一由随机生成保证,accepted 竞态仅剩理论可能,优先采纳 accepted)。
	var best *forwardReply
	anyDelivered := false
	for collected := 0; collected < len(candidates); collected++ {
		rep := <-replies
		if rep.transportErr {
			continue
		}
		anyDelivered = true
		if !rep.definitive {
			continue
		}
		if best == nil || (rep.accepted && !best.accepted) {
			r := rep
			best = &r
		}
		cancel()
	}
	if best == nil {
		if !anyDelivered {
			return forwardUnreachable, 0, nil, ""
		}
		return forwardMiss, 0, nil, ""
	}
	if best.accepted {
		f.storeOwner(token, best.peer.NodeID)
	}
	return forwardHit, best.status, best.body, best.peer.Name
}

// lookupOwner 返回缓存的 token 归属环境 nodeID(过期不算命中)。
func (f *WebhookForwarder) lookupOwner(token string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.owners[token]
	if !ok || time.Now().After(e.expiresAt) {
		return "", false
	}
	return e.nodeID, true
}

// renewOwner 刷新缓存项存活期(直达路径命中后)。
func (f *WebhookForwarder) renewOwner(token, nodeID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.owners[token] = webhookOwnerEntry{nodeID: nodeID, expiresAt: time.Now().Add(webhookOwnerTTL)}
}

// storeOwner 写入缓存项(容量防御:异常膨胀时整体清空,归属可经 fan-out 重建)。
func (f *WebhookForwarder) storeOwner(token, nodeID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.owners) >= 1024 {
		f.owners = make(map[string]webhookOwnerEntry)
	}
	f.owners[token] = webhookOwnerEntry{nodeID: nodeID, expiresAt: time.Now().Add(webhookOwnerTTL)}
}

// invalidateOwner 移除缓存项(归属失效/直达失败)。
func (f *WebhookForwarder) invalidateOwner(token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.owners, token)
}
