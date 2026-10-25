// sweeper.go:定时备份调度循环——按配置间隔触发 scheduled 备份。
// 模式对齐 registryhub.NewSweeper:间隔轮询(非 cron 表达式),错峰首跑,
// 出错仅记日志。触发即写 last_run_at(CreateBackup 异步执行,不等待完成)。
package backup

import (
	"context"
	"sync"
	"time"
)

// Sweeper 是定时备份器。
type Sweeper struct {
	svc      *Service
	interval time.Duration
	stop     chan struct{}
	wg       sync.WaitGroup
}

// NewSweeper 构造定时备份器。interval<=0 用默认 10 分钟(检查到点与否很轻)。
func NewSweeper(svc *Service, interval time.Duration) *Sweeper {
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	return &Sweeper{svc: svc, interval: interval, stop: make(chan struct{})}
}

// Start 启动循环(启动后 5 分钟首查,避开启动高峰)。
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
		}
		s.tick(ctx)
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.tick(ctx)
			}
		}
	}()
}

// Stop 停止循环。
func (s *Sweeper) Stop() {
	close(s.stop)
	s.wg.Wait()
}

// tick 检查是否到点:enabled && interval>0 && 距上次触发 ≥ interval。
func (s *Sweeper) tick(ctx context.Context) {
	cfg, err := s.svc.GetConfig(ctx)
	if err != nil {
		s.svc.logf("定时备份读配置失败(已跳过本轮):%v", err)
		return
	}
	if !cfg.Enabled || cfg.IntervalHours <= 0 {
		return
	}
	if cfg.LastRunAt != "" {
		last, perr := time.Parse(time.RFC3339, cfg.LastRunAt)
		if perr == nil {
			if time.Since(last) < time.Duration(cfg.IntervalHours)*time.Hour {
				return
			}
		}
	}
	// 先推进 last_run_at 再触发:执行是异步的,不推进会在下一轮重复触发。
	if err := s.svc.TouchLastRun(ctx); err != nil {
		s.svc.logf("定时备份写 last_run_at 失败(已跳过本轮):%v", err)
		return
	}
	if _, err := s.svc.CreateBackup(ctx, CreateInput{Kind: "scheduled"}); err != nil {
		s.svc.logf("定时备份触发失败:%v", err)
		return
	}
	s.svc.logf("定时备份已触发(间隔 %d 小时)", cfg.IntervalHours)
}
