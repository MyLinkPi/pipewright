// sweeper.go:实例定时备份循环——扫描两张实例表 interval>0 的到点实例,先推
// last_backup_at 再异步触发 scheduled 备份(模式与平台备份 sweeper 一致)。
// db_instances 里 mysql/postgres 同表,kind 取 engine 列;redis_instances 恒为 redis。
package dbbackup

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

// Start 启动循环(启动后 8 分钟首扫错峰);ctx 取消或 Stop 退出。
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

// due 是一个到点的待备份实例。
type due struct {
	kind, id string
}

// scanDue 扫两张实例表,返回到点(interval>0 且距 last ≥ interval)的实例。
func (s *Sweeper) scanDue(ctx context.Context) []due {
	now := time.Now()
	var out []due
	appendDue := func(kind, id, last string, interval int) {
		if interval <= 0 {
			return
		}
		if last != "" {
			if t, err := time.Parse(time.RFC3339, last); err == nil &&
				now.Sub(t) < time.Duration(interval)*time.Hour {
				return
			}
		}
		out = append(out, due{kind: kind, id: id})
	}

	rows, err := s.svc.db.QueryContext(ctx,
		`SELECT id, engine, backup_interval_hours, last_backup_at FROM db_instances WHERE backup_interval_hours > 0`)
	if err != nil {
		s.svc.logf("定时备份扫描失败(db_instances,已跳过本轮):%v", err)
	} else {
		for rows.Next() {
			var id, engine, last string
			var interval int
			if rows.Scan(&id, &engine, &interval, &last) == nil {
				appendDue(engine, id, last, interval)
			}
		}
		_ = rows.Close()
	}

	rows, err = s.svc.db.QueryContext(ctx,
		`SELECT id, backup_interval_hours, last_backup_at FROM redis_instances WHERE backup_interval_hours > 0`)
	if err != nil {
		s.svc.logf("定时备份扫描失败(redis_instances,已跳过本轮):%v", err)
	} else {
		for rows.Next() {
			var id, last string
			var interval int
			if rows.Scan(&id, &interval, &last) == nil {
				appendDue(KindRedis, id, last, interval)
			}
		}
		_ = rows.Close()
	}
	return out
}

// tick 扫描并触发(先推 last_backup_at 防下轮重复)。
func (s *Sweeper) tick(ctx context.Context) {
	for _, d := range s.scanDue(ctx) {
		if err := s.svc.TouchLastRun(ctx, d.kind, d.id); err != nil {
			continue
		}
		go func(kind, id string) {
			if _, err := s.svc.RunBackup(context.Background(), kind, id, "scheduled"); err != nil {
				s.svc.logf("定时备份失败(%s %s):%v", kind, id, err)
			}
		}(d.kind, d.id)
	}
}
