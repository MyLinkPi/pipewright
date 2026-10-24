package secscan

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/huangchengsir/pipewright/internal/cron"
)

// 定时规则(全局单行,id='default';缺省 = 关闭 + 每日 03:00 + trivy)。

// defaultScheduleExpression 是未保存过规则时的默认表达式(每日 03:00)。
const defaultScheduleExpression = "0 3 * * *"

// GetSchedule 读取定时规则;从未保存过 → 返回默认(未启用)。
func (s *service) GetSchedule(ctx context.Context) (*Schedule, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT enabled, expression, scan_types, last_run_at, updated_at FROM security_schedule WHERE id = ?`, scheduleRowID)
	var (
		enabled          int
		expr, typesRaw   string
		lastRun, updated sql.NullString
	)
	err := row.Scan(&enabled, &expr, &typesRaw, &lastRun, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return &Schedule{Enabled: false, Expression: defaultScheduleExpression, ScanTypes: []string{ScanTypeTrivy}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("secscan: get schedule: %w", err)
	}
	sch := &Schedule{
		Enabled:    enabled != 0,
		Expression: expr,
		ScanTypes:  []string{ScanTypeTrivy},
		UpdatedAt:  time.Now().UTC(),
	}
	_ = json.Unmarshal([]byte(typesRaw), &sch.ScanTypes)
	if len(sch.ScanTypes) == 0 {
		sch.ScanTypes = []string{ScanTypeTrivy}
	}
	if lastRun.Valid {
		if t, err := time.Parse(time.RFC3339, lastRun.String); err == nil {
			sch.LastRunAt = t
		}
	}
	if updated.Valid {
		if t, err := time.Parse(time.RFC3339, updated.String); err == nil {
			sch.UpdatedAt = t
		}
	}
	return sch, nil
}

// SaveSchedule 校验并保存定时规则(cron 表达式经 internal/cron 校验;扫描类型
// 至少一种)。表达式留空 = 恢复默认「0 3 * * *」。
func (s *service) SaveSchedule(ctx context.Context, in ScheduleInput) (*Schedule, error) {
	expr := strings.TrimSpace(in.Expression)
	if expr == "" {
		expr = defaultScheduleExpression
	}
	if err := cron.Valid(expr); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	types := normalizeScanTypes(in.ScanTypes)
	if len(types) == 0 {
		return nil, fmt.Errorf("%w: 至少选择一种扫描类型", ErrInvalidInput)
	}
	typesRaw, err := json.Marshal(types)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	enabled := 0
	if in.Enabled {
		enabled = 1
	}
	// 双方言通用 upsert:先 UPDATE,零行命中再 INSERT(并发窗口的唯一索引冲突吞掉重试)。
	res, err := s.db.ExecContext(ctx,
		`UPDATE security_schedule SET enabled = ?, expression = ?, scan_types = ?, updated_at = ? WHERE id = ?`,
		enabled, expr, string(typesRaw), now.Format(time.RFC3339), scheduleRowID)
	if err != nil {
		return nil, fmt.Errorf("secscan: save schedule: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO security_schedule (id, enabled, expression, scan_types, last_run_at, updated_at)
			 VALUES (?, ?, ?, ?, '', ?)`,
			scheduleRowID, enabled, expr, string(typesRaw), now.Format(time.RFC3339)); err != nil && !isUniqueViolation(err) {
			return nil, fmt.Errorf("secscan: insert schedule: %w", err)
		}
	}
	return s.GetSchedule(ctx)
}

// TouchScheduleLastRun 记录定时规则最近一次实际触发(调度器调用)。
func (s *service) TouchScheduleLastRun(ctx context.Context, at time.Time) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE security_schedule SET last_run_at = ? WHERE id = ?`,
		at.UTC().Format(time.RFC3339), scheduleRowID); err != nil {
		return fmt.Errorf("secscan: touch schedule: %w", err)
	}
	return nil
}

// normalizeScanTypes 去重、去非法值,保持 trivy 在前的稳定顺序。
func normalizeScanTypes(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range in {
		t = strings.TrimSpace(t)
		if !validScanType(t) || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	if len(out) == 2 && out[0] == ScanTypeNuclei {
		out[0], out[1] = out[1], out[0]
	}
	return out
}
