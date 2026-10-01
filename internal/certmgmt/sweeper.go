package certmgmt

import (
	"context"
	"log"
	"sync"
	"time"
)

// Sweeper 是证书自动续期调度器(照抄 retention.Sweeper 模式:延迟首扫 + 固定间隔 +
// recover-safe;区别仅在每轮调 SweepOnce 触发到期证书续期)。
type Sweeper struct {
	svc      Service
	interval time.Duration

	stop chan struct{}
	wg   sync.WaitGroup
}

// NewSweeper 构造调度器。interval<=0 时用默认 1 小时。未 Start 不起 goroutine。
func NewSweeper(svc Service, interval time.Duration) *Sweeper {
	if interval <= 0 {
		interval = time.Hour
	}
	return &Sweeper{svc: svc, interval: interval, stop: make(chan struct{})}
}

// Start 启动后台续期 goroutine:延迟 2 分钟首扫(避开启动高峰,也给「创建即签发」让路),
// 之后每 interval 扫一次。续期出错仅记日志,绝不影响平台。
func (s *Sweeper) Start(ctx context.Context) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		first := time.NewTimer(2 * time.Minute)
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
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[certmgmt] 续期轮询 panic 已恢复:%v", rec)
		}
	}()
	n, err := s.svc.SweepOnce(ctx)
	if err != nil {
		log.Printf("[certmgmt] 续期轮询失败:%v", err)
		return
	}
	if n > 0 {
		log.Printf("[certmgmt] 本轮触发 %d 张证书自动续期", n)
	}
}

// Stop 停止调度 goroutine(幂等)并等待退出。
func (s *Sweeper) Stop() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	s.wg.Wait()
}
