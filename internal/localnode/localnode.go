// Package localnode 是「本机(控制节点)角色」领域层:让运行 Pipewright 的控制机**可选启用**
// 为构建机 / 部署目标 / 网关主机(specs/local-node-roles.md)。
//
// 设计要点:
//   - 单行配置(id 恒 'local',仿 mesh_settings):三角色三开关,默认全关 —— 关闭态下
//     消费方(调度器候选注入 / 部署目标解析 / 网关主机编排)行为与改造前逐字节一致。
//   - 本机**永不**进 servers 表:本包只存角色开关与构建机池参数(labels/max_builds/priority,
//     与 servers 同语义),机器身份恒为哨兵 target.LocalHostID,由 target.WithLocalHostRoles
//     包装层路由到本机 os/exec(不经 SSH、无凭据面)。
//   - OS 门禁:部署与网关的全部脚本都是 POSIX 假设,deploy/gateway 角色仅 Linux 控制机可开;
//     build 角色不受限(本地构建走本地 driver 路径,与平台运行平台无关)。
//   - 本机仍是控制面:启用部署/网关角色 = 流水线动作获得控制机执行权(控制面 RCE 级信任),
//     仅 admin 可配、变更落审计(httpapi 层),UI 明示。
package localnode

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/huangchengsir/pipewright/internal/runner"
	"github.com/huangchengsir/pipewright/internal/store"
)

// localNodeID 是本机配置行的固定主键。单例哨兵,与 target.LocalHostID 的机器身份对齐
// (两者字面量不同是刻意的:一个是配置行主键,一个是机器伪 id;勿混用)。
const localNodeID = "local"

// 领域错误。
var (
	// ErrInvalidInput 表示入参非法(labels 语法/槽位/优先级越界)。
	ErrInvalidInput = errors.New("localnode: invalid input")
	// ErrPlatformUnsupported 表示当前控制机平台不支持所启用的角色(非 Linux 开
	// deploy/gateway):部署与网关的全部脚本都是 POSIX 假设(sh -c / tar / bash /dev/tcp /
	// curl / ss / ip route),诚实拒绝而非运行期半途失败。
	ErrPlatformUnsupported = errors.New("localnode: platform unsupported for deploy/gateway roles")
)

// Roles 是本机角色视图(单行;布尔与数值直接可读,无敏感字段)。
type Roles struct {
	// BuildEnabled 启用本机为构建机池成员(标签圈选/钉死,占槽、排队、亲和全套语义)。
	BuildEnabled bool
	// BuildLabels 是构建机池标签(与 servers.labels 同语法与上限;空 = 不参与标签圈选,
	// 仍可被 server:__local__ 钉死)。
	BuildLabels string
	// BuildMaxBuilds 是本机并发构建槽位(0 = 调度器全局默认;与 servers.max_builds 同语义)。
	// 只约束池内本机任务,不构成整机并发上限(空选择器直连本地构建不占槽)。
	BuildMaxBuilds int
	// BuildPriority 是调度优先级(0-100,数值越大越优先;与 servers.priority 同语义)。
	BuildPriority int
	// DeployEnabled 启用本机为部署目标(显式 serverIds / server:__local__ 钉死 / 标签圈选)。
	DeployEnabled bool
	// GatewayEnabled 启用本机为网关主机(Settings.ServerIDs 可含 __local__)。
	GatewayEnabled bool
}

// SaveInput 是保存本机角色的入参;指针字段为 nil 表示不修改。
type SaveInput struct {
	BuildEnabled   *bool
	BuildLabels    *string
	BuildMaxBuilds *int
	BuildPriority  *int
	DeployEnabled  *bool
	GatewayEnabled *bool
}

// PlatformSupportsDeployGateway 报告当前控制机是否支持部署/网关角色(仅 Linux)。
// 供 httpapi 视图层与前端能力位使用;保存路径的强校验见 ensurePlatform。
func PlatformSupportsDeployGateway() bool { return runtime.GOOS == "linux" }

// Service 是本机角色配置的读写接口。
type Service interface {
	// Get 返回当前角色视图;无行 = 全关(零行为,免初始化)。
	Get(ctx context.Context) (Roles, error)
	// Save 校验并保存角色视图;返回保存后的完整视图。OS 门禁在「最终态」上裁决:
	// 非 Linux 平台把 deploy/gateway 置开 → ErrPlatformUnsupported(已开着的不受影响,
	// 平台不变就不会出现该态)。
	Save(ctx context.Context, in SaveInput) (Roles, error)
}

type service struct {
	db *sql.DB
}

// New 构造 Service。
func New(db *sql.DB) Service { return &service{db: db} }

// ensurePlatform 裁决 OS 门禁(仅对被置开的角色;最终态语义)。
func ensurePlatform(r Roles) error {
	if (r.DeployEnabled || r.GatewayEnabled) && !PlatformSupportsDeployGateway() {
		return ErrPlatformUnsupported
	}
	return nil
}

// validate 归一并校验构建机池参数(与 servers 同口径):labels 走 runner.ValidateLabels
// (同一字符集/32 项/512 字符);max_builds 0..64(slotCapMax 对齐);priority 0..100。
func validate(r Roles) (Roles, error) {
	r.BuildLabels = strings.TrimSpace(r.BuildLabels)
	if err := runner.ValidateLabels(r.BuildLabels); err != nil {
		return Roles{}, fmt.Errorf("%w: build_labels: %w", ErrInvalidInput, err)
	}
	if r.BuildMaxBuilds < 0 || r.BuildMaxBuilds > 64 {
		return Roles{}, fmt.Errorf("%w: build_max_builds 须在 0..64", ErrInvalidInput)
	}
	if r.BuildPriority < 0 || r.BuildPriority > 100 {
		return Roles{}, fmt.Errorf("%w: build_priority 须在 0..100", ErrInvalidInput)
	}
	return r, nil
}

// rowQuerier 是角色读取所需的只读查询能力(*sql.DB 与 *sql.Tx 都满足),让 Get 与
// Save 的事务内读取共用同一段扫描逻辑。
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func getRoles(ctx context.Context, q rowQuerier) (Roles, error) {
	row := q.QueryRowContext(ctx,
		`SELECT COALESCE(build_enabled,0), COALESCE(build_labels,''), COALESCE(build_max_builds,0), COALESCE(build_priority,0),
		        COALESCE(deploy_enabled,0), COALESCE(gateway_enabled,0)
		 FROM local_node WHERE id = ?`, localNodeID)
	r, err := scanRoles(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Roles{}, nil // 无行 = 全关(默认态)
		}
		return Roles{}, fmt.Errorf("localnode: get: %w", err)
	}
	return r, nil
}

func (s *service) Get(ctx context.Context) (Roles, error) {
	return getRoles(ctx, s.db)
}

// Save 在单个事务内完成「读当前态 → 合并入参 → 校验 → upsert」:并发保存(多 admin 会话)
// 不会互相覆盖彼此的字段(Patch 语义依赖读-改-写原子性)。
func (s *service) Save(ctx context.Context, in SaveInput) (Roles, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Roles{}, fmt.Errorf("localnode: save: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	cur, err := getRoles(ctx, tx)
	if err != nil {
		return Roles{}, err
	}
	next := cur
	if in.BuildEnabled != nil {
		next.BuildEnabled = *in.BuildEnabled
	}
	if in.BuildLabels != nil {
		next.BuildLabels = *in.BuildLabels
	}
	if in.BuildMaxBuilds != nil {
		next.BuildMaxBuilds = *in.BuildMaxBuilds
	}
	if in.BuildPriority != nil {
		next.BuildPriority = *in.BuildPriority
	}
	if in.DeployEnabled != nil {
		next.DeployEnabled = *in.DeployEnabled
	}
	if in.GatewayEnabled != nil {
		next.GatewayEnabled = *in.GatewayEnabled
	}
	next, err = validate(next)
	if err != nil {
		return Roles{}, err
	}
	if err := ensurePlatform(next); err != nil {
		return Roles{}, err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	_, err = tx.ExecContext(ctx,
		`INSERT INTO local_node (id, build_enabled, build_labels, build_max_builds, build_priority, deploy_enabled, gateway_enabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) `+
			store.UpsertSuffix(store.DialectOf(s.db),
				[]string{"id"},
				[]string{"build_enabled", "build_labels", "build_max_builds", "build_priority", "deploy_enabled", "gateway_enabled", "updated_at"}),
		localNodeID, boolInt(next.BuildEnabled), next.BuildLabels, next.BuildMaxBuilds, next.BuildPriority,
		boolInt(next.DeployEnabled), boolInt(next.GatewayEnabled), now, now)
	if err != nil {
		return Roles{}, fmt.Errorf("localnode: save: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Roles{}, fmt.Errorf("localnode: save: %w", err)
	}
	return next, nil
}

// scanRoles 把一行(或 sql.Row)扫描为 Roles。
func scanRoles(sc interface{ Scan(dest ...any) error }) (Roles, error) {
	var r Roles
	var buildEn, deployEn, gatewayEn int
	if err := sc.Scan(&buildEn, &r.BuildLabels, &r.BuildMaxBuilds, &r.BuildPriority, &deployEn, &gatewayEn); err != nil {
		return Roles{}, err
	}
	r.BuildEnabled = buildEn != 0
	r.DeployEnabled = deployEn != 0
	r.GatewayEnabled = gatewayEn != 0
	return r, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
