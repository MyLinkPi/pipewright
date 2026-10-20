//go:build windows

package main

import "testing"

// TestAutoStartRoundTrip 验证 HKCU Run 项的写/读/删回路(运行后自动清理)。
func TestAutoStartRoundTrip(t *testing.T) {
	if err := setAutoStart(true); err != nil {
		t.Fatalf("setAutoStart(true): %v", err)
	}
	if !autoStartEnabled() {
		t.Fatal("setAutoStart(true) 后 autoStartEnabled 应为 true")
	}
	cmd := autoStartValue()
	want, err := autoStartCommand()
	if err != nil {
		t.Fatalf("autoStartCommand: %v", err)
	}
	if cmd != want {
		t.Fatalf("自启命令 = %q, want %q", cmd, want)
	}
	if err := setAutoStart(false); err != nil {
		t.Fatalf("setAutoStart(false): %v", err)
	}
	if autoStartEnabled() {
		t.Fatal("setAutoStart(false) 后 autoStartEnabled 应为 false")
	}
}
