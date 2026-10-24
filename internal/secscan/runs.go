package secscan

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// run 行的存取与聚合。summary 列是 JSON 文本;完整明细在 artifact store。

// artifactMaxBytes 是读取报告 JSON 的上限(单次扫描 findings 上万也远小于此)。
const artifactMaxBytes = 64 << 20

// insertRun 落一行 running 状态的 run。
func (s *service) insertRun(ctx context.Context, run *Run) error {
	raw, err := json.Marshal(run.Summary)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO scan_runs (id, server_id, server_name, scan_type, status, trigger_source, summary, artifact_key, error, scanner_instance_id, started_at, finished_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, '', '', ?, ?, '')`,
		run.ID, run.ServerID, run.ServerName, run.ScanType, run.Status, run.Trigger,
		string(raw), run.ScannerID, run.StartedAt.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("secscan: insert run: %w", err)
	}
	return nil
}

// finishRun 写终态(status = done|failed;artifactKey/error/summary 随终态落库)。
// 独立短超时上下文由调用方传入(扫描 ctx 可能已随服务关停取消,终态必须落库)。
func (s *service) finishRun(ctx context.Context, id, status string, summary Summary, artifactKey, errMsg string) error {
	raw, err := json.Marshal(summary)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE scan_runs SET status = ?, summary = ?, artifact_key = ?, error = ?, finished_at = ? WHERE id = ?`,
		status, string(raw), artifactKey, truncateRunes(errMsg, 2048), time.Now().UTC().Format(time.RFC3339), id); err != nil {
		return fmt.Errorf("secscan: finish run: %w", err)
	}
	return nil
}

// MarkStaleRuns 把遗留 running 行置 failed(服务重启后调用一次,诚实反映中断)。
func (s *service) MarkStaleRuns(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE scan_runs SET status = 'failed', error = ?, finished_at = ? WHERE status = 'running'`,
		"服务重启,扫描中断;可重新发起", time.Now().UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("secscan: mark stale runs: %w", err)
	}
	return nil
}

// Sweep 删除 before 之前的 run 行(artifact 文件不回收,属可接受的磁盘占用)。
func (s *service) Sweep(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM scan_runs WHERE started_at < ?`, before.UTC().Format(time.RFC3339))
	if err != nil {
		return 0, fmt.Errorf("secscan: sweep runs: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// runScanner 抽象行扫描。
type runScanner interface {
	Scan(dest ...any) error
}

// scanRun 把一行 scan_runs 扫描为 Run。
func scanRun(sc runScanner) (*Run, error) {
	var (
		run          Run
		summaryRaw   string
		startedRaw   string
		finishedRaw  sql.NullString
	)
	if err := sc.Scan(&run.ID, &run.ServerID, &run.ServerName, &run.ScanType, &run.Status,
		&run.Trigger, &summaryRaw, &run.ArtifactKey, &run.Error, &run.ScannerID,
		&startedRaw, &finishedRaw); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(summaryRaw), &run.Summary)
	var err error
	if run.StartedAt, err = time.Parse(time.RFC3339, startedRaw); err != nil {
		return nil, fmt.Errorf("secscan: parse started_at: %w", err)
	}
	if finishedRaw.Valid && finishedRaw.String != "" {
		if run.FinishedAt, err = time.Parse(time.RFC3339, finishedRaw.String); err != nil {
			return nil, fmt.Errorf("secscan: parse finished_at: %w", err)
		}
	}
	return &run, nil
}

const runColumns = `id, server_id, server_name, scan_type, status, trigger_source, summary, artifact_key, error, scanner_instance_id, started_at, finished_at`

// ListRuns 按条件查历史(started_at 倒序;limit 默认 50、上限 200)。
func (s *service) ListRuns(ctx context.Context, f RunFilter) ([]Run, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	if f.Limit > 200 {
		f.Limit = 200
	}
	q := `SELECT ` + runColumns + ` FROM scan_runs`
	var args []any
	conds := make([]string, 0, 3)
	if f.ServerID != "" {
		conds = append(conds, "server_id = ?")
		args = append(args, f.ServerID)
	}
	if f.ScanType != "" {
		if !validScanType(f.ScanType) {
			return nil, fmt.Errorf("%w: scan type", ErrInvalidInput)
		}
		conds = append(conds, "scan_type = ?")
		args = append(args, f.ScanType)
	}
	if len(conds) > 0 {
		q += " WHERE " + joinAnd(conds)
	}
	q += " ORDER BY started_at DESC LIMIT ?"
	args = append(args, f.Limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("secscan: list runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]Run, 0)
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *run)
	}
	return out, rows.Err()
}

// GetRun 读取单条;不存在 → ErrNotFound。
func (s *service) GetRun(ctx context.Context, id string) (*Run, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM scan_runs WHERE id = ?`, id)
	run, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return run, err
}

// latestRun 取某机某类型的最近一次(任意状态);无 → nil。
func (s *service) latestRun(ctx context.Context, serverID, scanType string) (*Run, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+runColumns+` FROM scan_runs WHERE server_id = ? AND scan_type = ? ORDER BY started_at DESC LIMIT 1`,
		serverID, scanType)
	run, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return run, err
}

// joinAnd 用 AND 连接条件(免 strings 依赖轮子;本文件语义直白)。
func joinAnd(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " AND "
		}
		out += p
	}
	return out
}

// RunFindings 读某次扫描的明细(artifact JSON → 内存过滤分页);run 无 artifact
//(running/failed/旧数据)→ 空页。全量计数随页返回供徽标渲染。
func (s *service) RunFindings(ctx context.Context, id string, f FindingFilter) (*FindingsPage, error) {
	run, err := s.GetRun(ctx, id)
	if err != nil {
		return nil, err
	}
	page := &FindingsPage{Items: []Finding{}}
	if s.artifacts == nil || run.ArtifactKey == "" {
		return page, nil
	}
	rc, err := s.artifacts.Open(run.ArtifactKey)
	if err != nil {
		return nil, fmt.Errorf("secscan: open report artifact: %w", err)
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(io.LimitReader(rc, artifactMaxBytes))
	if err != nil {
		return nil, fmt.Errorf("secscan: read report artifact: %w", err)
	}
	var doc ReportDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("secscan: parse report artifact: %w", err)
	}
	page.Summary = Summarize(doc.Findings)
	total, items := FilterFindings(doc.Findings, f)
	page.Total = total
	page.Items = items
	return page, nil
}

// Overview 聚合全员:每机每类型最新一次(任意状态,含 running)+ 各机最新 done 的
// 严重度合计。服务器清单来自 runner(排除 PVE 节点)。
func (s *service) Overview(ctx context.Context) (*Overview, error) {
	if s.runner == nil {
		return nil, ErrRunnerUnavailable
	}
	servers, err := s.runner.Servers(ctx)
	if err != nil {
		return nil, err
	}
	ov := &Overview{Servers: make([]OverviewServer, 0, len(servers))}
	for _, srv := range servers {
		if srv.IsPVE {
			continue
		}
		item := OverviewServer{ServerID: srv.ID, ServerName: srv.Name, Host: srv.Host}
		if run, err := s.latestRun(ctx, srv.ID, ScanTypeTrivy); err == nil && run != nil {
			item.Trivy = toRunLite(run)
			if run.Status == StatusDone {
				ov.Totals.Add(run.Summary)
			}
		}
		if run, err := s.latestRun(ctx, srv.ID, ScanTypeNuclei); err == nil && run != nil {
			item.Nuclei = toRunLite(run)
			if run.Status == StatusDone {
				ov.Totals.Add(run.Summary)
			}
		}
		if item.Trivy != nil || item.Nuclei != nil {
			if item.Trivy != nil && item.Trivy.Status == StatusDone || item.Nuclei != nil && item.Nuclei.Status == StatusDone {
				ov.Scanned++
			}
			latest := item.Trivy
			if item.Nuclei != nil && (latest == nil || item.Nuclei.StartedAt.After(latest.StartedAt)) {
				latest = item.Nuclei
			}
			if latest != nil && latest.FinishedAt.After(ov.LastScanAt) {
				ov.LastScanAt = latest.FinishedAt
			}
		}
		ov.Servers = append(ov.Servers, item)
	}
	return ov, nil
}

// toRunLite 把 Run 折成概览/历史行。
func toRunLite(r *Run) *RunLite {
	return &RunLite{
		RunID: r.ID, ScanType: r.ScanType, Status: r.Status, Trigger: r.Trigger,
		Summary: r.Summary, Error: r.Error, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
	}
}

// CollectReport 组装 HTML 审计报告数据:每台(或指定一台)服务器最近一次 done 的
// trivy 与 nuclei 完整报告;从未扫过 → 对应章节为 nil(模板渲染「未扫描」)。
func (s *service) CollectReport(ctx context.Context, serverIDs []string) ([]ServerReport, error) {
	if s.runner == nil {
		return nil, ErrRunnerUnavailable
	}
	var targets []ServerInfo
	if len(serverIDs) > 0 {
		seen := map[string]bool{}
		for _, id := range serverIDs {
			if seen[id] {
				continue // Service 是公开接口:直接调用方也要拿到去重语义(handler 已去重)
			}
			seen[id] = true
			srv, err := s.runner.Server(ctx, id)
			if errors.Is(err, ErrServerNotFound) {
				continue // 中途被删的机器跳过,不炸整份报告
			}
			if err != nil {
				return nil, err
			}
			targets = append(targets, srv)
		}
	} else {
		all, err := s.runner.Servers(ctx)
		if err != nil {
			return nil, err
		}
		for _, srv := range all {
			if !srv.IsPVE {
				targets = append(targets, srv)
			}
		}
	}
	out := make([]ServerReport, 0, len(targets))
	for _, srv := range targets {
		rep := ServerReport{ServerName: srv.Name, Host: srv.Host}
		if run, err := s.latestRun(ctx, srv.ID, ScanTypeTrivy); err == nil && run != nil && run.Status == StatusDone {
			rep.Trivy = &RunReport{FinishedAt: run.FinishedAt, Summary: run.Summary, Doc: s.loadDoc(ctx, run)}
		}
		if run, err := s.latestRun(ctx, srv.ID, ScanTypeNuclei); err == nil && run != nil && run.Status == StatusDone {
			rep.Nuclei = &RunReport{FinishedAt: run.FinishedAt, Summary: run.Summary, Doc: s.loadDoc(ctx, run)}
		}
		out = append(out, rep)
	}
	return out, nil
}

// loadDoc 从 artifact store 读某次 run 的完整报告;读不到 → nil(报告降级为仅计数)。
func (s *service) loadDoc(ctx context.Context, run *Run) *ReportDoc {
	if s.artifacts == nil || run.ArtifactKey == "" {
		return nil
	}
	rc, err := s.artifacts.Open(run.ArtifactKey)
	if err != nil {
		return nil
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(io.LimitReader(rc, artifactMaxBytes))
	if err != nil {
		return nil
	}
	var doc ReportDoc
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	SortFindings(doc.Findings)
	return &doc
}
