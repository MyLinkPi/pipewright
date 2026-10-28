package pwclient

import (
	"strings"
	"testing"
)

func TestLogRingTailAndSince(t *testing.T) {
	r := newLogRing(3)
	for _, line := range []string{"a", "b", "c"} {
		_, _ = r.Write([]byte(line + "\n"))
	}
	// 首次拉取:reset=true,next=3,三行。
	lines, next, reset := r.Since(0, 0)
	if !reset || next != 3 || strings.Join(lines, ",") != "a,b,c" {
		t.Fatalf("首拉不符: lines=%v next=%d reset=%v", lines, next, reset)
	}
	// 增量:只返回 seq>2 的行。
	lines, next, reset = r.Since(2, 0)
	if reset || next != 3 || strings.Join(lines, ",") != "c" {
		t.Fatalf("增量不符: lines=%v next=%d reset=%v", lines, next, reset)
	}
	// 无新增:空增量。
	lines, next, reset = r.Since(3, 0)
	if reset || next != 3 || len(lines) != 0 {
		t.Fatalf("无新增应空: lines=%v next=%d reset=%v", lines, next, reset)
	}
	// 环滚动:写 d/e 后最旧为 seq=3;since=1 已滚出 → reset。
	_, _ = r.Write([]byte("d\ne\n"))
	lines, next, reset = r.Since(1, 0)
	if !reset || next != 5 || strings.Join(lines, ",") != "c,d,e" {
		t.Fatalf("滚动后应 reset 且返回环内行: lines=%v next=%d reset=%v", lines, next, reset)
	}
	lines, next, reset = r.Since(3, 0)
	if reset || strings.Join(lines, ",") != "d,e" || next != 5 {
		t.Fatalf("滚动后增量不符: lines=%v next=%d reset=%v", lines, next, reset)
	}
	// max 截断取尾部。
	lines, _, _ = r.Since(0, 2)
	if strings.Join(lines, ",") != "d,e" {
		t.Fatalf("max 截断不符: %v", lines)
	}
	// Tail 保持文本语义。
	if got := strings.Join(r.Tail(2), ","); got != "d,e" {
		t.Fatalf("Tail 不符: %s", got)
	}
}

func TestLogRingEmpty(t *testing.T) {
	r := newLogRing(2)
	lines, next, reset := r.Since(0, 0)
	if len(lines) != 0 || next != 0 || !reset {
		t.Fatalf("空环首拉: lines=%v next=%d reset=%v", lines, next, reset)
	}
	lines, next, reset = r.Since(5, 0)
	if len(lines) != 0 || next != 0 || reset {
		t.Fatalf("空环非首拉: lines=%v next=%d reset=%v", lines, next, reset)
	}
}
