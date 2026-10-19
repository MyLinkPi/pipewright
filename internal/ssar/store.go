// store.go — ssar_samples 时序表的存取(仿 internal/metrics 纪律:纯 SQL,无 SSH 逻辑;
// 参数化语句;指标无敏感信息)。
package ssar

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// maxHistoryPoints 单次时序查询返回的最大点数(约 7 天 @ 1min)。
const maxHistoryPoints = 11000

// record 批量写入采样(每台一行;空切片 no-op)。
func (s *service) record(ctx context.Context, samples []Sample) error {
	if len(samples) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ssar: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO ssar_samples (id, server_id, sampled_at,
		    cpu_user, cpu_system, cpu_iowait, cpu_idle, cpu_steal,
		    mem_total_kb, mem_free_kb, mem_avail_kb,
		    load1, runq, threads,
		    tcp_curr_estab, tcp_retrans_segs, tcp_syn_retrans, tcp_fast_retrans, tcp_retrans_fail)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("ssar: prepare: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, sm := range samples {
		at := sm.At.UTC().Format(time.RFC3339)
		if _, err := stmt.ExecContext(ctx, uuid.NewString(), sm.ServerID, at,
			nullF(sm.CPUUser), nullF(sm.CPUSystem), nullF(sm.CPUIowait), nullF(sm.CPUIdle), nullF(sm.CPUSteal),
			nullF(sm.MemTotalKB), nullF(sm.MemFreeKB), nullF(sm.MemAvailKB),
			nullF(sm.Load1), nullF(sm.Runq), nullF(sm.Threads),
			nullF(sm.TCPCurrEstab), nullF(sm.TCPRetransSegs), nullF(sm.TCPSynRetrans), nullF(sm.TCPFastRetrans), nullF(sm.TCPRetransFail)); err != nil {
			return fmt.Errorf("ssar: insert sample: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ssar: commit: %w", err)
	}
	return nil
}

// History 返回该台自 since 起的采样点(升序;上限 maxHistoryPoints)。
func (s *service) History(ctx context.Context, serverID string, since time.Time) ([]Point, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT cpu_user, cpu_system, cpu_iowait, cpu_idle, cpu_steal,
		        mem_total_kb, mem_free_kb, mem_avail_kb,
		        load1, runq, threads,
		        tcp_curr_estab, tcp_retrans_segs, tcp_syn_retrans, tcp_fast_retrans, tcp_retrans_fail,
		        sampled_at
		   FROM ssar_samples
		  WHERE server_id = ? AND sampled_at >= ?
		  ORDER BY sampled_at ASC
		  LIMIT ?`,
		serverID, since.UTC().Format(time.RFC3339), maxHistoryPoints)
	if err != nil {
		return nil, fmt.Errorf("ssar: query history: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Point, 0)
	for rows.Next() {
		var (
			cols  [16]sql.NullFloat64
			atStr string
		)
		dest := make([]any, 0, 17)
		for i := range cols {
			dest = append(dest, &cols[i])
		}
		dest = append(dest, &atStr)
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("ssar: scan sample: %w", err)
		}
		at, err := time.Parse(time.RFC3339, atStr)
		if err != nil {
			return nil, fmt.Errorf("ssar: parse sampled_at: %w", err)
		}
		out = append(out, Point{
			At:             at,
			CPUUser:        fromNull(cols[0]),
			CPUSystem:      fromNull(cols[1]),
			CPUIowait:      fromNull(cols[2]),
			CPUIdle:        fromNull(cols[3]),
			CPUSteal:       fromNull(cols[4]),
			MemTotalKB:     fromNull(cols[5]),
			MemFreeKB:      fromNull(cols[6]),
			MemAvailKB:     fromNull(cols[7]),
			Load1:          fromNull(cols[8]),
			Runq:           fromNull(cols[9]),
			Threads:        fromNull(cols[10]),
			TCPCurrEstab:   fromNull(cols[11]),
			TCPRetransSegs: fromNull(cols[12]),
			TCPSynRetrans:  fromNull(cols[13]),
			TCPFastRetrans: fromNull(cols[14]),
			TCPRetransFail: fromNull(cols[15]),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ssar: iterate samples: %w", err)
	}
	return out, nil
}

// LastSampleAt 返回该台最近一次入库采样时间(无样本返回零值)。
func (s *service) LastSampleAt(ctx context.Context, serverID string) (time.Time, error) {
	var atStr string
	err := s.db.QueryRowContext(ctx,
		`SELECT sampled_at FROM ssar_samples WHERE server_id = ? ORDER BY sampled_at DESC LIMIT 1`,
		serverID).Scan(&atStr)
	if err == sql.ErrNoRows {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("ssar: query last sample: %w", err)
	}
	at, err := time.Parse(time.RFC3339, atStr)
	if err != nil {
		return time.Time{}, fmt.Errorf("ssar: parse sampled_at: %w", err)
	}
	return at, nil
}

// Sweep 删除早于 before 的样本,返回删除行数(保留清理)。
func (s *service) Sweep(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM ssar_samples WHERE sampled_at < ?`,
		before.UTC().Format(time.RFC3339))
	if err != nil {
		return 0, fmt.Errorf("ssar: sweep: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// nullF 把 *float64 转为可空 SQL 值(nil → NULL)。
func nullF(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

// fromNull 把可空 SQL 浮点转回 *float64(NULL → nil)。
func fromNull(n sql.NullFloat64) *float64 {
	if !n.Valid {
		return nil
	}
	v := n.Float64
	return &v
}
