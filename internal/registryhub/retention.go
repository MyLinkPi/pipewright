package registryhub

import (
	"context"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

// PruneResult 是按保留策略清理制品 registry 的结果。
type PruneResult struct {
	OK          bool     `json:"ok"`
	DeletedTags int      `json:"deletedTags"`
	Repos       []string `json:"repos"`    // 有删除发生的仓库名
	GCOutput    string   `json:"gcOutput"` // registry garbage-collect 输出尾部
	Error       string   `json:"error"`
}

// registryClient 按当前配置取制品 registry 客户端(测试经 opts 注入 fake;生产按端口自建,
// TLS 栈走 https + 本机探活客户端,见 localRegistryURL/probeHTTPClient)。
func (h *Hub) registryClient(ctx context.Context, cfg *Config) RegistryAPI {
	if h.opts.RegistryClient != nil {
		return h.opts.RegistryClient
	}
	return NewClient(h.localRegistryURL(cfg, cfg.ArtifactPort), h.probeHTTPClient(cfg))
}

// Prune 按保留策略裁剪制品 registry 的镜像 tag:
//   - keepPerProject>0:每个仓库按创建时间倒序保留最近 N 个,其余删除;
//   - maxAgeDays>0:创建时间早于 N 天的删除;
//   - 例外:名为 latest 的 tag 永久豁免(0059 迁移注释承诺;部署侧常按 :latest 固定引用)。
//   - 两条件取并集(与 run retention 语义一致:超龄或超量即删)。
//
// 删除经 registry API DELETE manifest(按 digest 生效;与保留 tag 共享 digest 的待删 tag
// 会跳过,避免连带误删);随后 best-effort 触发 garbage-collect 回收 blob。
// 未 enabled 或两条件均未配 → 直接返回(0 删除)。任何仓库/单 tag 失败都跳过并计入 Error,
// 绝不中断其余仓库的清理。
func (h *Hub) Prune(ctx context.Context, now time.Time) (*PruneResult, error) {
	cfg, err := h.Get(ctx)
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled || (cfg.KeepPerProject <= 0 && cfg.MaxAgeDays <= 0) {
		return &PruneResult{OK: true}, nil
	}
	cli := h.registryClient(ctx, cfg)
	repos, err := cli.Catalog(ctx)
	if err != nil {
		return &PruneResult{OK: false, Error: "读取制品仓库目录失败:" + err.Error()}, nil
	}

	var cutoff time.Time
	if cfg.MaxAgeDays > 0 {
		cutoff = now.UTC().AddDate(0, 0, -cfg.MaxAgeDays)
	}

	res := &PruneResult{OK: true}
	var errs []string
	for _, repo := range repos {
		infos, terr := cli.TagInfos(ctx, repo)
		if terr != nil {
			errs = append(errs, repo+": "+terr.Error())
			continue
		}
		// 创建时间倒序(新的在前);零值时间(取不到 created)排最后=最旧,倾向被清。
		sort.SliceStable(infos, func(i, j int) bool { return infos[i].Created.After(infos[j].Created) })
		// 先标记待删集合并收集「保留 tag 引用的 manifest digest」:DELETE manifest 按 digest
		// 生效,若待删 tag 与保留 tag 共享同一 digest,直接删会让保留 tag 连带失效——跳过。
		drop := make([]bool, len(infos))
		keptDigest := map[string]bool{}
		for rank, info := range infos {
			del := false
			if info.Tag != "latest" {
				// latest 永久豁免(0059 迁移注释承诺):它通常被部署侧按固定引用拉取,
				// 删了会让引用 <addr>/<slug>:latest 的部署 pull 必败。
				overCount := cfg.KeepPerProject > 0 && rank >= cfg.KeepPerProject
				tooOld := !cutoff.IsZero() && !info.Created.IsZero() && info.Created.Before(cutoff)
				del = overCount || tooOld
			}
			drop[rank] = del
			if !del && info.Digest != "" {
				keptDigest[info.Digest] = true
			}
		}
		deleted := false
		for i, info := range infos {
			if !drop[i] || keptDigest[info.Digest] {
				continue
			}
			if derr := cli.DeleteManifest(ctx, repo, info.Digest); derr != nil {
				errs = append(errs, repo+":"+info.Tag+" "+derr.Error())
				continue
			}
			res.DeletedTags++
			deleted = true
		}
		if deleted {
			res.Repos = append(res.Repos, repo)
		}
	}

	// 有删除才触发 gc(best-effort;在线 gc 有短暂窗口,失败留待下轮重试)。
	if res.DeletedTags > 0 {
		gcCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		_, stderr, code, gcErr := h.opts.Runner.Run(gcCtx, "docker",
			[]string{"exec", artifactCont, "registry", "garbage-collect", "/etc/docker/registry/config.yml"}, "")
		cancel()
		switch {
		case gcErr != nil || code != 0:
			out := strings.TrimSpace(stderr)
			if out == "" {
				out = "garbage-collect 未运行或以非零状态退出"
			}
			errs = append(errs, tail(out, 512))
		default:
			res.GCOutput = tail(strings.TrimSpace(stderr), 2048)
		}
	}
	if len(errs) > 0 {
		res.Error = tail(strings.Join(errs, "; "), 1024)
	}
	return res, nil
}

// PruneCount 供 Sweeper 复用的计数适配(同 retention.Pruner 形态)。
func (h *Hub) PruneCount(ctx context.Context, now time.Time) (int, error) {
	res, err := h.Prune(ctx, now)
	if err != nil {
		return 0, err
	}
	return res.DeletedTags, nil
}

// Sweeper 是 registry 保留策略的后台周期执行器(仿 retention.Sweeper 模式;平台第二个
// 周期维护任务,独立实例以便日志前缀与频率区别于 run retention)。
type Sweeper struct {
	hub      *Hub
	interval time.Duration

	stop chan struct{}
	wg   sync.WaitGroup
}

// NewSweeper 构造清理器;interval<=0 时默认 24 小时(gc 有窗口,低频执行)。
func NewSweeper(h *Hub, interval time.Duration) *Sweeper {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	return &Sweeper{hub: h, interval: interval, stop: make(chan struct{})}
}

// Start 启动后台 goroutine:启动延迟 5 分钟先跑一次,之后每 interval 一轮。清理失败仅记日志。
func (s *Sweeper) Start(ctx context.Context) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		first := time.NewTimer(5 * time.Minute)
		defer first.Stop()
		select {
		case <-s.stop:
			return
		case <-ctx.Done():
			return
		case <-first.C:
			s.runOnce(ctx)
		}
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.runOnce(ctx)
			}
		}
	}()
}

func (s *Sweeper) runOnce(ctx context.Context) {
	res, err := s.hub.Prune(ctx, time.Now())
	if err != nil {
		log.Printf("[registryhub] 镜像保留清理失败:%v", err)
		return
	}
	if res.DeletedTags > 0 {
		log.Printf("[registryhub] 已清理 %d 个过期镜像 tag(仓库:%s)", res.DeletedTags, strings.Join(res.Repos, ","))
	}
	if res.Error != "" {
		log.Printf("[registryhub] 镜像保留清理部分失败:%s", res.Error)
	}
}

// Stop 停止后台 goroutine(幂等)并等待退出。
func (s *Sweeper) Stop() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	s.wg.Wait()
}
