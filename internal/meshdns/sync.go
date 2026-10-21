// sync.go 是集群 DNS 映射的同步器:把每条启用映射的域名 A 记录 upsert 为本机虚拟 IP。
//
//   - 虚拟 IP 以 EasyTier 运行态 SelfIP 为准(兼容 DHCP 自动分配;托管/外部模式皆可),
//     运行态不可用时回退集群设置的 virtualIp(由 main 注入的 selfIP 提供器决定);
//   - 自动循环(30s 节拍):IP 变化 / 从未成功 / 上次失败 → 同步;距上次成功超
//     reEnsureInterval(12h)强制重同步一次,防云端记录被手工改动漂移;
//   - 失败退避:失败条目 5 分钟内不重试(单机条目少,但别拿 30s 节拍 hammer 提供商 API);
//     手动 SyncNow 不受退避限制;
//   - 单条同步带独立超时,失败只记该条 last_error,不影响其余条目。
package meshdns

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"
)

const (
	// syncTick 是自动循环节拍。
	syncTick = 30 * time.Second
	// ensureTimeout 是单条 A 记录 upsert 的独立超时(提供商 API 可能慢)。
	ensureTimeout = 20 * time.Second
	// reEnsureInterval 是「IP 未变也强制重同步」的兜底周期。
	reEnsureInterval = 12 * time.Hour
	// failBackoff 是失败条目的自动重试退避。
	failBackoff = 5 * time.Minute
)

// SyncResult 是一次同步(单条)的结果(SyncNow 逐条返回给前端)。
type SyncResult struct {
	ID    string `json:"id"`
	FQDN  string `json:"fqdn"`
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// Syncer 驱动映射同步。零值不可用,经 NewSyncer 构造。
type Syncer struct {
	svc Service
	src ProviderSource
	// selfIP 返回本机当前虚拟 IP(空 = 尚不可用:集群未启用 / EasyTier 未就绪)。
	selfIP func(ctx context.Context) string
	// meshEnabled 报告集群是否启用(未启用时自动循环空转,不碰任何记录)。
	meshEnabled func(ctx context.Context) bool

	// mu 保护 failRetry(自动循环节拍与手动 SyncNow 并发回写)。
	mu sync.Mutex
	// failRetry 记录失败条目的下次允许自动重试时刻(进程内,重启即清)。
	failRetry map[string]time.Time
}

// NewSyncer 构造同步器。selfIP / meshEnabled 由 main 注入(读 EasyTier 运行态与集群设置)。
func NewSyncer(svc Service, src ProviderSource, selfIP func(ctx context.Context) string, meshEnabled func(ctx context.Context) bool) *Syncer {
	return &Syncer{svc: svc, src: src, selfIP: selfIP, meshEnabled: meshEnabled, failRetry: map[string]time.Time{}}
}

// Run 启动自动同步循环(随 ctx 停机)。
func (s *Syncer) Run(ctx context.Context) {
	ticker := time.NewTicker(syncTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if s.meshEnabled != nil && !s.meshEnabled(ctx) {
				continue
			}
			s.syncAuto(ctx)
		}
	}
}

// syncAuto 执行一轮自动同步:仅处理「需要同步」的条目(IP 变化/从未成功/上次失败/
// 兜底周期到),失败条目受退避限制。虚拟 IP 不可用时静默跳过(等下个节拍)。
func (s *Syncer) syncAuto(ctx context.Context) {
	ip := s.currentIP(ctx)
	if ip == "" {
		return
	}
	records, err := s.svc.List(ctx)
	if err != nil {
		log.Printf("[meshdns] 读取映射失败:%v", err)
		return
	}
	now := time.Now()
	for _, r := range records {
		if !r.Enabled {
			continue
		}
		if !needsSync(r, ip, now) {
			continue
		}
		if s.retryAfter(r.ID).After(now) {
			continue
		}
		s.ensureOne(ctx, r, ip, false)
	}
}

// SyncNow 手动同步全部启用条目(设置页「立即同步」按钮;同步执行,逐条返回结果,
// 不受失败退避限制)。返回当前虚拟 IP(空 = 不可用,results 为空)与逐条结果。
func (s *Syncer) SyncNow(ctx context.Context) (string, []SyncResult) {
	ip := s.currentIP(ctx)
	if ip == "" {
		return "", nil
	}
	records, err := s.svc.List(ctx)
	if err != nil {
		return ip, []SyncResult{{OK: false, Error: err.Error()}}
	}
	results := make([]SyncResult, 0, len(records))
	for _, r := range records {
		if !r.Enabled {
			continue
		}
		results = append(results, s.ensureOne(ctx, r, ip, true))
	}
	return ip, results
}

// ensureOne 同步单条记录并回写状态;返回结果(SyncNow 聚合用)。
func (s *Syncer) ensureOne(ctx context.Context, r Record, ip string, manual bool) SyncResult {
	res := SyncResult{ID: r.ID, FQDN: r.FQDN}
	ectx, cancel := context.WithTimeout(ctx, ensureTimeout)
	err := s.src.EnsureSubdomainRecord(ectx, r.ProviderID, r.FQDN, ip)
	cancel()
	if err == nil {
		res.OK = true
		if merr := s.svc.MarkSynced(ctx, r.ID, ip); merr != nil {
			log.Printf("[meshdns] %s 同步成功但回写状态失败:%v", r.FQDN, merr)
		}
		s.clearRetry(r.ID)
		return res
	}
	res.Error = err.Error()
	if merr := s.svc.MarkSyncFailed(ctx, r.ID, err.Error()); merr != nil {
		log.Printf("[meshdns] %s 同步失败且回写状态失败:%v", r.FQDN, merr)
	}
	if !manual {
		s.setRetry(r.ID)
	}
	log.Printf("[meshdns] %s A 记录同步失败(→ %s):%v", r.FQDN, ip, err)
	return res
}

// currentIP 取本机虚拟 IP(运行态 SelfIP 优先、配置值回退由注入的提供器决定;无 → 空)。
func (s *Syncer) currentIP(ctx context.Context) string {
	if s.selfIP == nil {
		return ""
	}
	return strings.TrimSpace(s.selfIP(ctx))
}

// needsSync 报告条目是否需要同步:IP 变化 / 从未成功 / 上次失败(含提供商根区变动后的
// 持续失败重试)/ 距上次成功超兜底周期。
func needsSync(r Record, ip string, now time.Time) bool {
	if r.LastIP != ip || r.LastSyncedAt == "" || r.LastError != "" {
		return true
	}
	last, err := time.Parse(time.RFC3339, r.LastSyncedAt)
	if err != nil {
		return true
	}
	return now.Sub(last) >= reEnsureInterval
}

func (s *Syncer) retryAfter(id string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failRetry[id]
}

func (s *Syncer) setRetry(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failRetry[id] = time.Now().Add(failBackoff)
}

func (s *Syncer) clearRetry(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.failRetry, id)
}
