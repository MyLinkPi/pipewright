package tokenest

import "testing"

func TestEstimate(t *testing.T) {
	cases := []struct {
		name string
		in   string
		min  int
		max  int
	}{
		{"empty", "", 0, 0},
		{"ascii", "hello world", 2, 4},   // 11 bytes → ~3
		{"cjk", "你好世界", 4, 4},            // 4 字 → 4 token
		{"mixed", "disk usage 你好", 5, 7}, // ascii 11B→3 + cjk 2 字→2 = 5
		{"digits", "12345678", 2, 2},     // 8 bytes → 2
		{"kana", "こんにちは", 5, 5},          // 5 字 → 5
	}
	for _, c := range cases {
		got := Estimate(c.in)
		if got < c.min || got > c.max {
			t.Errorf("%s: Estimate(%q) = %d, want [%d,%d]", c.name, c.in, got, c.min, c.max)
		}
	}
}

func TestEstimateMonotonic(t *testing.T) {
	// 追加内容绝不应减少估算。
	base := Estimate("prefix")
	if Estimate("prefix-more-text-longer") < base {
		t.Error("estimate decreased with more content")
	}
}
