// fileadmin_test.go — HTTP 面离线测试:fake Service 注入,覆盖鉴权(未登录 401/
// 普通成员 403)、目录列表/文本读取/流式下载/保存( CSRF)端点与领域错误码映射。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/auth"
	"github.com/huangchengsir/pipewright/internal/fileadmin"
	"github.com/huangchengsir/pipewright/internal/identity"
)

// fakeFileAdmin 按 canned 值实现 Service(未设置的方法返回零值)。
type fakeFileAdmin struct {
	fileadmin.Service

	listing *fileadmin.DirListing
	listErr error

	text    *fileadmin.TextFile
	readErr error

	writeBackup string
	writeErr    error
	wrotePath   string
	wroteBody   string

	downloadData string
	downloadErr  error
}

func (f *fakeFileAdmin) List(_ context.Context, _ string, _ string) (*fileadmin.DirListing, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.listing != nil {
		return f.listing, nil
	}
	return &fileadmin.DirListing{Path: "/", Entries: []fileadmin.DirEntry{}}, nil
}

func (f *fakeFileAdmin) Read(_ context.Context, _ string, _ string) (*fileadmin.TextFile, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	if f.text != nil {
		return f.text, nil
	}
	return &fileadmin.TextFile{}, nil
}

func (f *fakeFileAdmin) Write(_ context.Context, _ string, path, content string) (string, error) {
	f.wrotePath = path
	f.wroteBody = content
	return f.writeBackup, f.writeErr
}

func (f *fakeFileAdmin) Download(_ context.Context, _ string, _ string) (io.ReadCloser, int64, error) {
	if f.downloadErr != nil {
		return nil, 0, f.downloadErr
	}
	return io.NopCloser(strings.NewReader(f.downloadData)), int64(len(f.downloadData)), nil
}

// setupFileAdminAPI 构造挂了文件查看端点的测试 server(admin/testpass)。
func setupFileAdminAPI(t *testing.T, fake fileadmin.Service) (*httptest.Server, *http.Client, string) {
	t.Helper()
	st := testStoreAuth(t)
	authSvc := auth.NewService(st.DB, nil)
	if err := authSvc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	srv := httptest.NewServer(New(testWebFSAuth(), authSvc, WithFileAdmin(fake)))
	t.Cleanup(srv.Close)
	client := newTestClient(t)
	csrf := loginWithClient(t, client, srv.URL)
	return srv, client, csrf
}

func TestFileAdminListRequiresAuth(t *testing.T) {
	srv, _, _ := setupFileAdminAPI(t, &fakeFileAdmin{})
	resp, err := http.Get(srv.URL + "/api/fileadmin/servers/s1/files?path=/var/log")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未登录应 401, got %d", resp.StatusCode)
	}
}

func TestFileAdminMemberForbidden(t *testing.T) {
	st := testStoreAuth(t)
	authSvc := auth.NewService(st.DB, nil)
	if err := authSvc.Bootstrap("admin", "testpass", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	idSvc := identity.New(st.DB)
	srv := httptest.NewServer(New(testWebFSAuth(), authSvc, WithIdentity(idSvc), WithFileAdmin(&fakeFileAdmin{})))
	t.Cleanup(srv.Close)

	registerMember(t, idSvc, "file_member", "password123")
	member := newTestClient(t)
	loginAs(t, member, srv.URL, "file_member", "password123")

	resp, err := member.Get(srv.URL + "/api/fileadmin/servers/s1/files?path=/var/log")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("普通成员应 403, got %d", resp.StatusCode)
	}
}

func TestFileAdminListOK(t *testing.T) {
	fake := &fakeFileAdmin{listing: &fileadmin.DirListing{
		Path: "/var/log",
		Entries: []fileadmin.DirEntry{
			{Name: "nginx", Path: "/var/log/nginx", Type: "dir", Size: 4096, Mode: "755", Owner: "root", Group: "root", Mtime: 1710000000},
			{Name: "syslog", Path: "/var/log/syslog", Type: "file", Size: 2048, Mode: "644", Owner: "syslog", Group: "adm", Mtime: 1710000001},
		},
	}}
	srv, client, _ := setupFileAdminAPI(t, fake)
	resp := doJSON(t, client, http.MethodGet, srv.URL+"/api/fileadmin/servers/s1/files?path=/var/log", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list 应 200, got %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	var dto fileadmin.DirListing
	if err := json.Unmarshal(raw, &dto); err != nil {
		t.Fatalf("decode: %v body=%s", err, raw)
	}
	if dto.Path != "/var/log" || len(dto.Entries) != 2 || dto.Entries[0].Name != "nginx" || dto.Entries[1].Size != 2048 {
		t.Fatalf("列表 DTO 不符: %s", raw)
	}
}

func TestFileAdminMissingPathParam(t *testing.T) {
	srv, client, _ := setupFileAdminAPI(t, &fakeFileAdmin{})
	resp := doJSON(t, client, http.MethodGet, srv.URL+"/api/fileadmin/servers/s1/files", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("缺 path 应 400, got %d", resp.StatusCode)
	}
}

func TestFileAdminReadOK(t *testing.T) {
	fake := &fakeFileAdmin{text: &fileadmin.TextFile{
		Path: "/etc/app.conf", Size: 11, Editable: true, Content: "hello\nworld",
	}}
	srv, client, _ := setupFileAdminAPI(t, fake)
	resp := doJSON(t, client, http.MethodGet, srv.URL+"/api/fileadmin/servers/s1/file?path=/etc/app.conf", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read 应 200, got %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	var tf fileadmin.TextFile
	if err := json.Unmarshal(raw, &tf); err != nil {
		t.Fatalf("decode: %v body=%s", err, raw)
	}
	if !tf.Editable || tf.Content != "hello\nworld" {
		t.Fatalf("文本 DTO 不符: %s", raw)
	}
}

func TestFileAdminDownloadHeadersAndBody(t *testing.T) {
	fake := &fakeFileAdmin{downloadData: "line1\nline2\n"}
	srv, client, _ := setupFileAdminAPI(t, fake)
	resp := doJSON(t, client, http.MethodGet, srv.URL+"/api/fileadmin/servers/s1/file/download?path=/var/log/app.log", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download 应 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, `attachment; filename="app.log"`) {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "12" {
		t.Errorf("Content-Length = %q", cl)
	}
	raw, _ := io.ReadAll(resp.Body)
	if string(raw) != "line1\nline2\n" {
		t.Errorf("body = %q", raw)
	}
}

func TestFileAdminWriteSaves(t *testing.T) {
	fake := &fakeFileAdmin{writeBackup: "/etc/app.conf.pipewright-bak.20260101T000000Z"}
	srv, client, csrf := setupFileAdminAPI(t, fake)
	resp := doJSON(t, client, http.MethodPut, srv.URL+"/api/fileadmin/servers/s1/file", csrf,
		`{"path":"/etc/app.conf","content":"new content"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("write 应 200, got %d", resp.StatusCode)
	}
	if fake.wrotePath != "/etc/app.conf" || fake.wroteBody != "new content" {
		t.Errorf("写入参数不符: path=%q content=%q", fake.wrotePath, fake.wroteBody)
	}
	raw, _ := io.ReadAll(resp.Body)
	var dto struct {
		OK     bool   `json:"ok"`
		Backup string `json:"backup"`
	}
	if err := json.Unmarshal(raw, &dto); err != nil {
		t.Fatalf("decode: %v body=%s", err, raw)
	}
	if !dto.OK || dto.Backup != "/etc/app.conf.pipewright-bak.20260101T000000Z" {
		t.Fatalf("write DTO 不符: %s", raw)
	}
}

func TestFileAdminWriteRequiresCSRF(t *testing.T) {
	srv, client, _ := setupFileAdminAPI(t, &fakeFileAdmin{})
	resp := doJSON(t, client, http.MethodPut, srv.URL+"/api/fileadmin/servers/s1/file", "",
		`{"path":"/etc/app.conf","content":"x"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("缺 CSRF 应 403, got %d", resp.StatusCode)
	}
}

func TestFileAdminErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		fake   *fakeFileAdmin
		url    string
		method string
		status int
		code   string
	}{
		{
			name:   "list_not_found",
			fake:   &fakeFileAdmin{listErr: errors.Join(fileadmin.ErrNotFound, errors.New("no such"))},
			url:    "/api/fileadmin/servers/s1/files?path=/no/such",
			method: http.MethodGet,
			status: http.StatusNotFound, code: "file_not_found",
		},
		{
			name:   "list_not_dir",
			fake:   &fakeFileAdmin{listErr: errors.Join(fileadmin.ErrNotDir, errors.New("/etc/hosts"))},
			url:    "/api/fileadmin/servers/s1/files?path=/etc/hosts",
			method: http.MethodGet,
			status: http.StatusUnprocessableEntity, code: "file_not_dir",
		},
		{
			name:   "read_not_regular",
			fake:   &fakeFileAdmin{readErr: errors.Join(fileadmin.ErrNotFile, errors.New("/var/log"))},
			url:    "/api/fileadmin/servers/s1/file?path=/var/log",
			method: http.MethodGet,
			status: http.StatusUnprocessableEntity, code: "file_not_regular",
		},
		{
			name:   "download_exec_failed",
			fake:   &fakeFileAdmin{downloadErr: errors.Join(fileadmin.ErrExec, errors.New("ssh dial"))},
			url:    "/api/fileadmin/servers/s1/file/download?path=/var/log/x.log",
			method: http.MethodGet,
			status: http.StatusBadGateway, code: "file_exec_failed",
		},
		{
			name:   "write_invalid_input",
			fake:   &fakeFileAdmin{writeErr: errors.Join(fileadmin.ErrInvalidInput, errors.New("路径须为绝对路径"))},
			url:    "/api/fileadmin/servers/s1/file",
			method: http.MethodPut,
			status: http.StatusBadRequest, code: "invalid_file_input",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, client, csrf := setupFileAdminAPI(t, c.fake)
			resp := doJSON(t, client, c.method, srv.URL+c.url, csrf, `{"path":"x","content":"y"}`)
			defer resp.Body.Close()
			if resp.StatusCode != c.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, c.status)
			}
			raw, _ := io.ReadAll(resp.Body)
			var e struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(raw, &e); err != nil {
				t.Fatalf("decode: %v body=%s", err, raw)
			}
			if e.Error.Code != c.code {
				t.Fatalf("code = %s, want %s (body=%s)", e.Error.Code, c.code, raw)
			}
		})
	}
}
