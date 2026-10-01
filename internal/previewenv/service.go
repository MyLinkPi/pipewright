package previewenv

import (
	"context"
	"database/sql"
	"log"
	"strings"
	"time"
)

// validBaseDomain 抽象「校验根域 FQDN」的能力(由上层用 dnsprovider.ValidBaseDomain 注入,避免本包
// import dnsprovider 形成环;不注入则退回内置宽松校验)。
type validBaseDomainFunc func(s string) bool

// Allocator 抽象「在某根域下为预览分配子域名(建 A 记录)」的能力。
// 由上层用 dnsprovider.Service 适配注入(避免本包 import dnsprovider 形成环)。
// Caddy 反代下线后预览只保留 DNS 分配;流量路由(预览域名 → 上游)由网关体系另行承接。
type Allocator interface {
	// Allocate 为 providerID 下为 in.Subdomain 建指向 hostIP 的 A 记录。
	// hostIP 由上层据 server 解析得出(非用户自由文本)。失败返回错误(由调用方记日志,不阻断部署)。
	Allocate(ctx context.Context, in AllocateInput) error
}

// AllocateInput 是预览子域名分配入参(allocator 适配 dnsprovider 消费)。
type AllocateInput struct {
	ProviderID string
	// Subdomain 是要分配的完整 FQDN(pr-<n>-<proj>.base)。
	Subdomain string
	// HostIP 是 A 记录指向的宿主机公网 IP。
	HostIP string
}

// RecordDeleter 抽象「删某 DNS 提供商根域下一个 FQDN 的 A 记录」的能力(回收预览环境时清 DNS 解析)。
// 由上层用 dnsprovider.Service 适配注入;未绑则回收不清 DNS(优雅降级,仅删路由 + 标记)。
type RecordDeleter interface {
	DeleteRecord(ctx context.Context, providerID, fqdn string) error
}

// ZoneCoverer 抽象「校验某 FQDN 是否落在 DNS 提供商托管的某个根区下」的能力(多根区模型:
// 预览配置的 baseDomain 须落在所选提供商的某个根区下)。由上层用 dnsprovider.Service 适配注入;
// 未注入则跳过覆盖校验(仅做格式校验,优雅降级)。
type ZoneCoverer interface {
	ZoneCovers(ctx context.Context, providerID, fqdn string) (covered, ok bool, err error)
}

// service 是 store(+ 可选 allocator / recordDeleter / zoneCoverer / baseDomain 校验)
// 支撑的 Service 实现。
type service struct {
	store         *Store
	allocator     Allocator
	recordDeleter RecordDeleter
	zoneCoverer   ZoneCoverer
	validBase     validBaseDomainFunc
}

// New 构造 Service。allocator 等依赖可在装配后晚绑(SetAllocator 等),
// 以保持 New 签名稳定且避免 import 环;未绑时 provision/reclaim 优雅降级(no-op / 仅标记)。
func New(db *sql.DB) *service {
	return &service{store: NewStore(db), validBase: defaultValidBase}
}

// SetAllocator 注入预览子域名分配器(R4 E4.1:复用 dnsprovider.AllocateFQDN 建 A 记录)。
func (s *service) SetAllocator(a Allocator) { s.allocator = a }

// SetRecordDeleter 注入 DNS 记录删除器(回收预览环境时清 A 记录;未绑则不清 DNS)。
func (s *service) SetRecordDeleter(d RecordDeleter) { s.recordDeleter = d }

// SetZoneCoverer 注入根区覆盖校验器(预览配置 baseDomain 须落在所选提供商某个根区下;未绑则跳过)。
func (s *service) SetZoneCoverer(c ZoneCoverer) { s.zoneCoverer = c }

// SetBaseDomainValidator 注入根域校验器(默认用内置宽松校验;上层可换成 dnsprovider.ValidBaseDomain)。
func (s *service) SetBaseDomainValidator(f validBaseDomainFunc) {
	if f != nil {
		s.validBase = f
	}
}

func (s *service) List(ctx context.Context, projectID string) ([]PreviewEnv, error) {
	return s.store.list(ctx, strings.TrimSpace(projectID))
}

func (s *service) Get(ctx context.Context, id string) (*PreviewEnv, error) {
	return s.store.get(ctx, strings.TrimSpace(id))
}

func (s *service) GetConfig(ctx context.Context, projectID string) (*Config, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return nil, ErrInvalidProject
	}
	c, err := s.store.getConfig(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if c == nil {
		// 无配置 → 返回零值(未启用)。
		return &Config{ProjectID: projectID, Enabled: false}, nil
	}
	return c, nil
}

func (s *service) SetConfig(ctx context.Context, in Config) (*Config, error) {
	in.ProjectID = strings.TrimSpace(in.ProjectID)
	in.DNSProviderID = strings.TrimSpace(in.DNSProviderID)
	in.BaseDomain = strings.ToLower(strings.TrimSpace(in.BaseDomain))
	if in.ProjectID == "" {
		return nil, ErrInvalidProject
	}
	// 开启时必须给齐 DNS 提供商 + 合法根域(否则 provision 无从分配);且根域须落在所选
	// 提供商托管的某个根区下(多根区模型,如 preview.example.com 落在根区 example.com 下)。
	if in.Enabled {
		if in.DNSProviderID == "" {
			return nil, ErrConfigMissingProvider
		}
		if !s.validBase(in.BaseDomain) {
			return nil, ErrConfigInvalidBaseDomain
		}
		if s.zoneCoverer != nil {
			covered, ok, err := s.zoneCoverer.ZoneCovers(ctx, in.DNSProviderID, in.BaseDomain)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, ErrConfigMissingProvider
			}
			if !covered {
				return nil, ErrConfigInvalidBaseDomain
			}
		}
	}
	if err := s.store.upsertConfig(ctx, in); err != nil {
		return nil, err
	}
	return s.store.getConfig(ctx, in.ProjectID)
}

// Reclaim 回收某项目某 PR 的预览环境:清 DNS A 记录(best-effort)+ 标记 reclaimed。
// 无 active 环境 → ErrNotFound(调用方据情况静默,如 sweep 跳过)。
// (Caddy 反代下线后不再有路由可删;历史行的 route_id 仅作存档展示。)
func (s *service) Reclaim(ctx context.Context, projectID string, prNumber int) error {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return ErrInvalidProject
	}
	if prNumber < 1 {
		return ErrInvalidPR
	}
	env, err := s.store.getActiveByPR(ctx, projectID, prNumber)
	if err != nil {
		return err
	}
	// 清 DNS A 记录(best-effort:provision 时建了 pr-<n>-<proj>.base 的 A 记录,回收须一并删除,
	// 否则解析记录悬挂累积。取项目预览配置里的 DNS 提供商 + 本环境子域名;任何失败仅记日志不阻断标记)。
	if s.recordDeleter != nil && strings.TrimSpace(env.Subdomain) != "" {
		if cfg, cerr := s.store.getConfig(ctx, projectID); cerr == nil && cfg != nil && strings.TrimSpace(cfg.DNSProviderID) != "" {
			if derr := s.recordDeleter.DeleteRecord(ctx, cfg.DNSProviderID, env.Subdomain); derr != nil {
				log.Printf("[previewenv] 回收 PR #%d:删 DNS 记录 %s 失败(不阻断,请手动清):%v", prNumber, env.Subdomain, derr)
			}
		}
	}
	return s.store.markReclaimed(ctx, env.ID, time.Now().UTC())
}

// SweepReclaim 执行一轮自动回收:遍历全部 active 预览环境,对每个用 checker 查其 PR 状态;
// **仅当确证 closed / merged** 时回收(删路由 + 标记 reclaimed);open / unknown / 查询出错 一律跳过。
// 返回本轮回收条数。安全铁律:绝不回收未能确证已关闭/合并的环境(checker 自身对一切不确定回 unknown)。
//
// 配置门控:仅回收**所属项目仍开启预览**的环境;项目未开启预览(配置缺失 / Enabled=false)→ 跳过
// (该环境的路由实际已不再由预览逻辑续建,留给手动回收 / 重新开启后处理,绝不在此误删)。
// 幂等:已是 reclaimed 的环境不在 active 列表内,天然不重复回收;回收过程中环境消失(并发)→ 静默跳过。
func (s *service) SweepReclaim(ctx context.Context, checker PRStateChecker) (int, error) {
	if checker == nil {
		return 0, nil
	}
	envs, err := s.store.listActive(ctx)
	if err != nil {
		return 0, err
	}
	reclaimed := 0
	for i := range envs {
		env := envs[i]
		// 配置门控:项目未开启预览 → 跳过(不在此误删历史环境)。
		cfg, cerr := s.store.getConfig(ctx, env.ProjectID)
		if cerr != nil || cfg == nil || !cfg.Enabled {
			continue
		}
		state, serr := checker.State(ctx, env.ProjectID, env.PRNumber)
		if serr != nil || !state.IsClosedOrMerged() {
			// 查询出错 / open / unknown:绝不回收。
			continue
		}
		if rerr := s.Reclaim(ctx, env.ProjectID, env.PRNumber); rerr != nil {
			// 回收失败(含并发已被回收 → ErrNotFound):跳过本条,不计数。
			continue
		}
		reclaimed++
		log.Printf("[previewenv] 自动回收预览环境:project=%s pr=%d subdomain=%s state=%s",
			env.ProjectID, env.PRNumber, env.Subdomain, state)
	}
	return reclaimed, nil
}

// defaultValidBase 是内置宽松根域校验(无 dnsprovider 依赖时兜底):至少一个点 + 非空。
func defaultValidBase(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s != "" && len(s) <= 253 && strings.Contains(s, ".") &&
		!strings.HasPrefix(s, ".") && !strings.HasSuffix(s, ".") && !strings.Contains(s, " ")
}
