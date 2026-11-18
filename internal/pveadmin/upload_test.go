package pveadmin

// upload_test.go — .vma 上传版本自适应:9.x SSH 落盘 / 8.x API multipart /
// 非文件型存储报错 / readonly 闸门 / 文件名白名单。

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

// TestUploadBackup9xSSH 9.x API 不收 backup → SSH 流式写 dump 目录,返回绝对路径。
func TestUploadBackup9xSSH(t *testing.T) {
	s, ft, fr, fc := newTestService(t)
	cl := seedCreateCluster(t, s, ft)
	wireCreateFake(fc, "9.0.3")
	volid, err := s.UploadBackup(context.Background(), cl.ID, "pve1", "local", "test.vma", strings.NewReader("VMA-BYTES"))
	if err != nil {
		t.Fatalf("UploadBackup: %v", err)
	}
	if volid != "/var/lib/vz/dump/test.vma" {
		t.Fatalf("volid = %q", volid)
	}
	if len(fr.writes) != 1 || !strings.HasPrefix(fr.writes[0].path, "/var/lib/vz/dump/test.vma.pipewright-") || !strings.HasSuffix(fr.writes[0].path, ".part") || fr.writes[0].mode != 0o640 || fr.writes[0].content != "VMA-BYTES" {
		t.Fatalf("writes: %+v", fr.writes)
	}
	published := false
	for _, cmd := range fr.runs {
		if len(cmd) == 4 && cmd[0] == "ln" && cmd[2] == fr.writes[0].path && cmd[3] == "/var/lib/vz/dump/test.vma" {
			published = true
		}
	}
	if !published {
		t.Fatalf("上传未以临时文件原子发布: %v", fr.runs)
	}
	if len(fc.uploads) != 0 {
		t.Fatalf("9.x 不应走 API 上传: %v", fc.uploads)
	}
}

// TestUploadBackup8xAPI 8.x 存储启用 backup → API multipart,返回卷 ID。
func TestUploadBackup8xAPI(t *testing.T) {
	s, ft, fr, fc := newTestService(t)
	cl := seedCreateCluster(t, s, ft)
	wireCreateFake(fc, "8.2.4")
	fc.uploadFn = func(path string, fields url.Values, fileName string) (any, error) {
		if !strings.HasSuffix(path, "/nodes/pve1/storage/local/content") || fields.Get("content") != "backup" || fileName != "test.vma" {
			return nil, fmt.Errorf("unexpected upload %s %v", path, fields)
		}
		return "local:backup/test.vma", nil
	}
	volid, err := s.UploadBackup(context.Background(), cl.ID, "pve1", "local", "test.vma", strings.NewReader("VMA"))
	if err != nil || volid != "local:backup/test.vma" {
		t.Fatalf("UploadBackup = %q / %v", volid, err)
	}
	if len(fr.writes) != 0 {
		t.Fatalf("8.x API 路径不应 SSH 写盘: %+v", fr.writes)
	}
}

// TestUploadBackup8xNoBackupContent 8.x 存储未启用 backup 内容 → SSH 文件路径。
func TestUploadBackup8xNoBackupContent(t *testing.T) {
	s, ft, fr, fc := newTestService(t)
	cl := seedCreateCluster(t, s, ft)
	wireCreateFake(fc, "8.2.4")
	inner := fc.getFn
	fc.getFn = func(path string) (any, error) {
		if strings.Contains(path, "/nodes/") && strings.HasSuffix(path, "/storage") {
			return []map[string]any{{"storage": "local", "type": "dir", "active": 1, "enabled": 1, "content": "images,snippets"}}, nil
		}
		return inner(path)
	}
	volid, err := s.UploadBackup(context.Background(), cl.ID, "pve1", "local", "test.vma.zst", strings.NewReader("VMA"))
	if err != nil || volid != "/var/lib/vz/dump/test.vma.zst" {
		t.Fatalf("UploadBackup = %q / %v", volid, err)
	}
	if len(fr.writes) != 1 {
		t.Fatalf("writes: %+v", fr.writes)
	}
}

// TestUploadBackupNonFileStorage 9.x 非文件型存储(无 path)→ 可操作错误。
func TestUploadBackupNonFileStorage(t *testing.T) {
	s, ft, _, fc := newTestService(t)
	cl := seedCreateCluster(t, s, ft)
	wireCreateFake(fc, "9.0.3")
	inner := fc.getFn
	fc.getFn = func(path string) (any, error) {
		if strings.HasSuffix(path, "/storage/local") {
			return map[string]any{"type": "lvmthin"}, nil
		}
		return inner(path)
	}
	_, err := s.UploadBackup(context.Background(), cl.ID, "pve1", "local", "test.vma", strings.NewReader("VMA"))
	if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "文件型") {
		t.Fatalf("err = %v", err)
	}
}

// TestUploadBackupReadonlyAndValidation readonly 闸门 + 文件名白名单。
func TestUploadBackupReadonlyAndValidation(t *testing.T) {
	s, _, _, _ := newTestService(t)
	cl := seedCluster(t, s)
	seedReadyToken(t, s, cl.ID) // readonly 默认 1
	_, err := s.UploadBackup(context.Background(), cl.ID, "pve1", "local", "test.vma", strings.NewReader("VMA"))
	if !errors.Is(err, ErrReadonly) {
		t.Fatalf("readonly err = %v", err)
	}
	_ = s.SetReadonly(context.Background(), cl.ID, false)
	for _, name := range []string{"x.exe", "lxc.tar.zst", "../evil.vma", "has space.vma", ""} {
		if _, err := s.UploadBackup(context.Background(), cl.ID, "pve1", "local", name, strings.NewReader("VMA")); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("filename %q err = %v, want ErrInvalidInput", name, err)
		}
	}
}
