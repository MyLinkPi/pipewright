package opschat

import (
	"errors"
	"fmt"
	"testing"
)

// 一次续期失败(数据库锁抖动/IO 卡顿)不应终止服务;连续 3 个周期(≈租约有效期)
// 仍失败才终止;租约被其他实例接管立即终止。
func TestLeaseLost(t *testing.T) {
	busy := fmt.Errorf("%w: database is locked (5) (SQLITE_BUSY)", ErrStorage)
	for _, tc := range []struct {
		name  string
		err   error
		fails int
		want  bool
	}{
		{"首次存储错误-重试", busy, 0, false},
		{"第二次存储错误-重试", busy, 1, false},
		{"第三次存储错误-终止", busy, 2, true},
		{"被其他实例接管-立即终止", ErrLease, 0, true},
		{"裸存储错误-重试", ErrStorage, 0, false},
	} {
		if got := leaseLost(tc.err, tc.fails); got != tc.want {
			t.Errorf("%s: leaseLost(%v, %d) = %v, want %v", tc.name, tc.err, tc.fails, got, tc.want)
		}
	}
	// 续期处包装后的错误必须保持 ErrStorage 语义(errors.Is),供重试路径识别。
	if !errors.Is(busy, ErrStorage) {
		t.Error("wrapped storage error lost ErrStorage identity")
	}
	if errors.Is(busy, ErrLease) {
		t.Error("wrapped storage error must not be treated as lease takeover")
	}
}
