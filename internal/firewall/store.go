// store.go 持久化守卫单行(参数化 SQL,sqlite/mysql 两方言一致;端口逗号分隔、
// 网段换行分隔,与 mesh peers/proxy_networks 同模式)。
package firewall

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/huangchengsir/pipewright/internal/store"
)

// rowID 是 firewall_guard 的单行主键(全库仅此一行)。
const rowID = "default"

// ErrStoreNotFound 表示设置行不存在(store 层语义;对外经 getOrCreate 兜底,不外泄)。
var ErrStoreNotFound = errors.New("firewall: settings row not found")

// Store 持久化防火墙守卫单行。
type Store struct {
	db           *sql.DB
	defaultPorts []int // 惰性建行时的草稿默认端口
}

// NewStore 构造持久层。defaultPorts 为草稿惰性默认(如 80,443,5000,5001,8080)。
func NewStore(db *sql.DB, defaultPorts []int) *Store {
	return &Store{db: db, defaultPorts: defaultPorts}
}

// getOrCreate 读取单例;不存在则插入默认行(未开启)后回读(幂等)。
func (s *Store) getOrCreate(ctx context.Context) (*State, error) {
	st, err := s.get(ctx)
	if err == nil {
		return st, nil
	}
	if !errors.Is(err, ErrStoreNotFound) {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, ierr := s.db.ExecContext(ctx,
		`INSERT INTO firewall_guard
		   (id, ports, extra_cidrs, phase, last_result, status_detail, created_at, updated_at)
		 VALUES (?, ?, '', ?, '', '', ?, ?)`,
		rowID, joinPorts(s.defaultPorts), PhaseOff, now, now)
	if ierr != nil {
		// 并发初始化撞唯一键 → 回读即可。
		if !store.IsUniqueErr(ierr) {
			return nil, fmt.Errorf("firewall: insert settings: %w", ierr)
		}
	}
	return s.get(ctx)
}

// get 读取单例;不存在 → ErrStoreNotFound。
func (s *Store) get(ctx context.Context) (*State, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT ports, extra_cidrs, phase, pending_ports, pending_cidrs, pending_applied_at,
		        confirmed_ports, confirmed_cidrs, last_result, status_detail, last_applied_at,
		        created_at, updated_at
		 FROM firewall_guard WHERE id = ?`, rowID)
	var (
		st                                    State
		ports, extraCidrs                     string
		pendingPorts, pendingCidrs, pendingAt string
		confirmedPorts, confirmedCidr         string
		lastApply                             sql.NullString
		createdStr, updatedStr                string
	)
	if err := row.Scan(
		&ports, &extraCidrs, &st.Phase, &pendingPorts, &pendingCidrs, &pendingAt,
		&confirmedPorts, &confirmedCidr, &st.LastResult, &st.StatusDetail, &lastApply,
		&createdStr, &updatedStr,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrStoreNotFound
		}
		return nil, fmt.Errorf("firewall: scan settings: %w", err)
	}
	st.Ports = parsePorts(ports)
	st.ExtraCIDRs = splitLines(extraCidrs)
	st.PendingPorts = parsePorts(pendingPorts)
	st.PendingCIDRs = splitLines(pendingCidrs)
	st.ConfirmedPorts = parsePorts(confirmedPorts)
	st.ConfirmedCIDRs = splitLines(confirmedCidr)
	if pendingAt != "" {
		if t, err := time.Parse(time.RFC3339, pendingAt); err == nil {
			st.PendingAppliedAt = t
		}
	}
	if lastApply.Valid && lastApply.String != "" {
		if t, err := time.Parse(time.RFC3339, lastApply.String); err == nil {
			st.LastAppliedAt = t
		}
	}
	st.CreatedAt = parseTime(createdStr)
	st.UpdatedAt = parseTime(updatedStr)
	return &st, nil
}

// saveDraft 落库草稿(只动 ports/extra_cidrs;阶段与快照不动)。
func (s *Store) saveDraft(ctx context.Context, st *State) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx,
		`UPDATE firewall_guard SET ports = ?, extra_cidrs = ?, updated_at = ? WHERE id = ?`,
		joinPorts(st.Ports), strings.Join(st.ExtraCIDRs, "\n"), now, rowID)
	if err != nil {
		return fmt.Errorf("firewall: save draft: %w", err)
	}
	return nil
}

// setPending 记录一次临时生效(pending 快照 + phase + 最近结果;last_applied_at 一并刷新,
// 供 UI 展示剩余确认窗口)。
func (s *Store) setPending(ctx context.Context, ports []int, cidrs []string, detail string, appliedAt time.Time) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx,
		`UPDATE firewall_guard
		 SET phase = ?, pending_ports = ?, pending_cidrs = ?, pending_applied_at = ?,
		     last_result = ?, status_detail = ?, last_applied_at = ?, updated_at = ?
		 WHERE id = ?`,
		PhasePending, joinPorts(ports), strings.Join(cidrs, "\n"), appliedAt.UTC().Format(time.RFC3339),
		resultOK, detail, appliedAt.UTC().Format(time.RFC3339), now, rowID)
	if err != nil {
		return fmt.Errorf("firewall: set pending: %w", err)
	}
	return nil
}

// setConfirmed 晋升固化(pending 快照 → confirmed;phase=active)。
func (s *Store) setConfirmed(ctx context.Context, ports []int, cidrs []string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx,
		`UPDATE firewall_guard
		 SET phase = ?, confirmed_ports = ?, confirmed_cidrs = ?, last_result = ?,
		     status_detail = '', updated_at = ?
		 WHERE id = ?`,
		PhaseActive, joinPorts(ports), strings.Join(cidrs, "\n"), resultOK, now, rowID)
	if err != nil {
		return fmt.Errorf("firewall: set confirmed: %w", err)
	}
	return nil
}

// setPhase 记录一次回退结果:有固化版 → active(已重放固化版),否则 off(已清空)。
func (s *Store) setPhase(ctx context.Context, confirmedPorts []int, detail string) error {
	phase := PhaseOff
	if len(confirmedPorts) > 0 {
		phase = PhaseActive
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx,
		`UPDATE firewall_guard
		 SET phase = ?, last_result = ?, status_detail = ?, updated_at = ?
		 WHERE id = ?`,
		phase, resultReverted, detail, now, rowID)
	if err != nil {
		return fmt.Errorf("firewall: set phase: %w", err)
	}
	return nil
}

// markResult 回写最近一次后台结果(KeepAlive 断言失败等;不动阶段与快照)。
func (s *Store) markResult(ctx context.Context, result, detail string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx,
		`UPDATE firewall_guard SET last_result = ?, status_detail = ?, updated_at = ? WHERE id = ?`,
		result, detail, now, rowID)
	if err != nil {
		return fmt.Errorf("firewall: mark result: %w", err)
	}
	return nil
}

// setDisabled 复位为未开启(草稿保留,便于再次启用;两份快照与状态一并清空)。
func (s *Store) setDisabled(ctx context.Context) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx,
		`UPDATE firewall_guard
		 SET phase = ?, pending_ports = '', pending_cidrs = '', pending_applied_at = '',
		     confirmed_ports = '', confirmed_cidrs = '', last_result = ?, status_detail = '',
		     updated_at = ?
		 WHERE id = ?`,
		PhaseOff, resultOK, now, rowID)
	if err != nil {
		return fmt.Errorf("firewall: set disabled: %w", err)
	}
	return nil
}

// parseTime 宽松解析 RFC3339 时间串(空/坏值 → 零值)。
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

// parsePorts 解析逗号分隔端口串(空串 → nil;坏段忽略)。
func parsePorts(s string) []int {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []int
	for _, part := range strings.Split(s, ",") {
		if p, err := strconv.Atoi(strings.TrimSpace(part)); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// splitLines 解析换行分隔列表(空串 → nil;去空白行)。
func splitLines(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}
