// sweeper.go:DBaaS 定时备份循环——按实例 backup_interval_hours 扫描到点即触发。
// 触发即推进 last_backup_at(备份异步执行);单实例同一时刻仅一个备份(inflight 互斥)。
package dbaas

import (
	"context"
	"time"
)

// Sweeper 是定时备份器。
type Sweeper struct {
	svc      *Service
	interval time.Duration
	stop     chan struct{}
	done     chan struct{}
}

// NewSweeper 构造(interval<=0 用默认 10 分钟)。
func NewSweeper(svc *Service, interval time.Duration) *Sweeper {
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	return &Sweeper{svc: svc, interval: interval, stop: make(chan struct{}), done: make(chan struct{})}
}

// Start 启动循环(启动后 8 分钟首扫,错峰);ctx 取消或 Stop 退出。
func (s *Sweeper) Start(ctx context.Context) {
	go func() {
		defer close(s.done)
		first := time.NewTimer(8 * time.Minute)
		defer first.Stop()
		select {
		case <-s.stop:
			return
		case <-ctx.Done():
			return
		case <-first.C:
		}
		s.tick(ctx)
		t := time.NewTicker(s.interval)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				s.tick(ctx)
			}
		}
	}()
}

// Stop 停止并等待退出。
func (s *Sweeper) Stop() {
	close(s.stop)
	<-s.done
}

// tick 扫描启用定时备份的实例并到点触发。
func (s *Sweeper) tick(ctx context.Context) {
	rows, err := s.svc.db.QueryContext(ctx,
		`SELECT id, backup_interval_hours, last_backup_at FROM dbaas_instances WHERE backup_interval_hours > 0`)
	if err != nil {
		s.svc.logf("定时备份扫描失败(已跳过本轮):%v", err)
		return
	}
	type due struct {
		id       string
		interval int
		last     string
	}
	var list []due
	for rows.Next() {
		var d due
		if rows.Scan(&d.id, &d.interval, &d.last) == nil {
			list = append(list, d)
		}
	}
	_ = rows.Close()

	for _, d := range list {
		if d.last != "" {
			last, perr := time.Parse(time.RFC3339, d.last)
			if perr == nil && time.Since(last) < time.Duration(d.interval)*time.Hour {
				continue
			}
		}
		// 先推进 last_backup_at 再异步触发,防下轮重复。
		if _, err := s.svc.db.ExecContext(ctx,
			`UPDATE dbaas_instances SET last_backup_at = ? WHERE id = ?`,
			time.Now().UTC().Format(time.RFC3339), d.id); err != nil {
			continue
		}
		go func(id string) {
			if _, err := s.svc.RunBackup(context.Background(), id, "scheduled"); err != nil {
				s.svc.logf("定时备份失败(%s):%v", id, err)
			}
		}(d.id)
	}
}
