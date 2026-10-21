package ai

import "testing"

func TestNormalizeContext(t *testing.T) {
	on := true
	cases := []struct {
		name string
		in   ContextSettings
		want ContextSettings
	}{
		{"zero keeps defaults", ContextSettings{}, ContextSettings{AutoCompact: nil}},
		{"window in range", ContextSettings{WindowTokens: 128_000}, ContextSettings{WindowTokens: 128_000, AutoCompact: nil}},
		{"window too small", ContextSettings{WindowTokens: 500}, ContextSettings{AutoCompact: nil}},
		{"window too large", ContextSettings{WindowTokens: 9_000_000}, ContextSettings{AutoCompact: nil}},
		{"negative everything", ContextSettings{WindowTokens: -1, MaxOutputTokens: -5, HistoryMessages: -2, HistoryBytes: -3, KeepRecentTokens: -4, BufferTokens: -6, AutoCompact: &on}, ContextSettings{AutoCompact: &on}},
		{"keep above half window", ContextSettings{WindowTokens: 100_000, KeepRecentTokens: 90_000}, ContextSettings{WindowTokens: 100_000, AutoCompact: nil}},
		{"buffer above window", ContextSettings{WindowTokens: 100_000, BufferTokens: 100_000}, ContextSettings{WindowTokens: 100_000, AutoCompact: nil}},
		{"buffer below window ok", ContextSettings{WindowTokens: 100_000, BufferTokens: 8_000}, ContextSettings{WindowTokens: 100_000, BufferTokens: 8_000, AutoCompact: nil}},
	}
	for _, c := range cases {
		got := normalizeContext(c.in)
		if got.WindowTokens != c.want.WindowTokens || got.MaxOutputTokens != c.want.MaxOutputTokens ||
			got.HistoryMessages != c.want.HistoryMessages || got.HistoryBytes != c.want.HistoryBytes ||
			got.KeepRecentTokens != c.want.KeepRecentTokens || got.BufferTokens != c.want.BufferTokens {
			t.Errorf("%s: normalize(%+v) = %+v, want %+v", c.name, c.in, got, c.want)
		}
		if (got.AutoCompact == nil) != (c.want.AutoCompact == nil) {
			t.Errorf("%s: autocompact pointer mismatch", c.name)
		}
	}
}

func TestContextSettingsResolved(t *testing.T) {
	off := false
	r := ContextSettings{AutoCompact: &off}.Resolved()
	if r.WindowTokens != DefaultContextWindowTokens || r.MaxOutputTokens != DefaultMaxOutputTokens ||
		r.HistoryMessages != DefaultHistoryMessages || r.HistoryBytes != DefaultHistoryBytes ||
		r.KeepRecentTokens != DefaultContextKeepRecentTokens {
		t.Fatalf("defaults not filled: %+v", r)
	}
	if r.AutoCompact == nil || *r.AutoCompact {
		t.Fatal("explicit autocompact=false must be preserved")
	}
	if r.BufferTokens != 0 {
		t.Fatal("buffer stays 0 (=auto) after resolve")
	}
}
