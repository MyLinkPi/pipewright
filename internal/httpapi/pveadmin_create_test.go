package httpapi

// pveadmin_create_test.go — PVE 建机 HTTP 面契约:受理/只读拒绝/Busy 映射 + 审计
// (拒绝也留痕、detail 绝无凭据)、任务快照、只读下拉、aplinfo 下载、.vma 流式上传。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/pveadmin"
)

// doMultipart 发 multipart 上传(文件字段 filename;CSRF 同 doJSON)。
func doMultipart(t *testing.T, client *http.Client, url, csrf, fileName, content string) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("filename", fileName)
	if err != nil {
		t.Fatalf("form file: %v", err)
	}
	_, _ = fw.Write([]byte(content))
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPost, url, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("multipart %s: %v", url, err)
	}
	return resp
}

// TestPVECreateAccepted 建机受理 → 202 {ok:true} + 审计(detail 含 mode/name,绝无密码)。
func TestPVECreateAccepted(t *testing.T) {
	stub := &stubPVEAdmin{}
	base, client, csrf, rec := setupPVEAdminAPI(t, stub)

	resp := doJSON(t, client, http.MethodPost, base+"/api/pve/clusters/cl-1/create", csrf,
		`{"mode":"image","node":"pve1","name":"web","storage":"local","imageVolid":"local:import/x.qcow2","ciPassword":"SUPER-SECRET-PW","count":2}`)
	raw := readBody(t, resp)
	if resp.StatusCode != http.StatusAccepted || !strings.Contains(string(raw), `"ok":true`) {
		t.Fatalf("create = %d body=%s, want 202 ok", resp.StatusCode, raw)
	}
	if stub.createIn.Mode != "image" || stub.createIn.Count != 2 || stub.createIn.CIPassword != "SUPER-SECRET-PW" {
		t.Fatalf("入参透传: %+v", stub.createIn)
	}
	entries, err := rec.List(context.Background(), audit.ListFilter{Action: auditActionPVEVMCreate})
	if err != nil || len(entries.Entries) != 1 {
		t.Fatalf("audit = %d / %v, want 1", len(entries.Entries), err)
	}
	detail := fmt.Sprint(entries.Entries[0].Detail)
	if strings.Contains(detail, "SUPER-SECRET-PW") {
		t.Fatalf("审计泄漏密码: %s", detail)
	}
	if entries.Entries[0].Detail["mode"] != "image" || entries.Entries[0].Detail["name"] != "web" {
		t.Fatalf("audit detail: %+v", entries.Entries[0].Detail)
	}
}

// TestPVECreateReadonlyAndBusy 只读拒绝 → 409 留审计;Busy → 409 pve_busy。
func TestPVECreateReadonlyAndBusy(t *testing.T) {
	stub := &stubPVEAdmin{createErr: fmt.Errorf("%w: 只读", pveadmin.ErrReadonly)}
	base, client, csrf, rec := setupPVEAdminAPI(t, stub)
	resp := doJSON(t, client, http.MethodPost, base+"/api/pve/clusters/cl-1/create", csrf, `{"mode":"clone","name":"x","templateVmid":9000}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("readonly create = %d, want 409", resp.StatusCode)
	}
	entries, _ := rec.List(context.Background(), audit.ListFilter{Action: auditActionPVEVMCreate})
	if len(entries.Entries) != 1 {
		t.Fatalf("只读拒绝应留审计: %d", len(entries.Entries))
	}

	stub2 := &stubPVEAdmin{createErr: fmt.Errorf("%w: 占用", pveadmin.ErrBusy)}
	base2, client2, csrf2, _ := setupPVEAdminAPI(t, stub2)
	resp = doJSON(t, client2, http.MethodPost, base2+"/api/pve/clusters/cl-1/create", csrf2, `{"mode":"clone","name":"x","templateVmid":9000}`)
	raw := readBody(t, resp)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(raw), "pve_busy") {
		t.Fatalf("busy = %d body=%s, want 409 pve_busy", resp.StatusCode, raw)
	}
}

// TestPVECreateStatus 快照透传(步骤/逐台结果)。
func TestPVECreateStatus(t *testing.T) {
	stub := &stubPVEAdmin{status: pveadmin.CreateStatus{
		Running: false, Done: true, Mode: "clone", Step: "prepare", Total: 2, OK: 1, Failed: 1,
		Items: []pveadmin.CreateItemResult{{Index: 0, VMID: 100, Name: "web-1", OK: true}, {Index: 1, VMID: 101, Name: "web-2", Err: "disk full"}},
	}}
	base, client, csrf, _ := setupPVEAdminAPI(t, stub)
	resp := doJSON(t, client, http.MethodGet, base+"/api/pve/clusters/cl-1/create-status", csrf, "")
	raw := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var st pveadmin.CreateStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !st.Done || st.OK != 1 || st.Failed != 1 || len(st.Items) != 2 || st.Items[1].Err != "disk full" {
		t.Fatalf("snapshot: %+v", st)
	}
}

// TestPVEProvisionReadEndpoints 只读下拉 200 形状;缺 node → 400。
func TestPVEProvisionReadEndpoints(t *testing.T) {
	stub := &stubPVEAdmin{}
	base, client, csrf, _ := setupPVEAdminAPI(t, stub)
	for _, path := range []string{
		"/api/pve/clusters/cl-1/templates",
		"/api/pve/clusters/cl-1/storages?node=pve1",
		"/api/pve/clusters/cl-1/bridges?node=pve1",
		"/api/pve/clusters/cl-1/nextid",
		"/api/pve/clusters/cl-1/storage-content?node=pve1&storage=local&content=vztmpl",
		"/api/pve/clusters/cl-1/aplinfo?node=pve1",
	} {
		resp := doJSON(t, client, http.MethodGet, base+path, csrf, "")
		raw := readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d body=%s", path, resp.StatusCode, raw)
		}
	}
	resp := doJSON(t, client, http.MethodGet, base+"/api/pve/clusters/cl-1/storages", csrf, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("缺 node = %d, want 400", resp.StatusCode)
	}
	resp = doJSON(t, client, http.MethodGet, base+"/api/pve/clusters/cl-1/nextid", csrf, "")
	raw := readBody(t, resp)
	if !strings.Contains(string(raw), `"vmid":100`) {
		t.Fatalf("nextid body=%s", raw)
	}
}

// TestPVEAplDownloadContract 下载受理 → 202 {node,upid} + 审计;只读 → 409 留审计。
func TestPVEAplDownloadContract(t *testing.T) {
	stub := &stubPVEAdmin{aplRef: &pveadmin.TaskRef{Node: "pve1", UPID: "UPID:pve1:ABC:1:download:0:"}}
	base, client, csrf, rec := setupPVEAdminAPI(t, stub)
	resp := doJSON(t, client, http.MethodPost, base+"/api/pve/clusters/cl-1/aplinfo/download", csrf,
		`{"node":"pve1","storage":"local","template":"ubuntu-24.04-standard"}`)
	raw := readBody(t, resp)
	if resp.StatusCode != http.StatusAccepted || !strings.Contains(string(raw), "UPID:") {
		t.Fatalf("apl download = %d body=%s", resp.StatusCode, raw)
	}
	entries, _ := rec.List(context.Background(), audit.ListFilter{Action: auditActionPVEAplDownload})
	if len(entries.Entries) != 1 {
		t.Fatalf("audit = %d, want 1", len(entries.Entries))
	}

	stub2 := &stubPVEAdmin{aplErr: fmt.Errorf("%w: 只读", pveadmin.ErrReadonly)}
	base2, client2, csrf2, rec2 := setupPVEAdminAPI(t, stub2)
	resp = doJSON(t, client2, http.MethodPost, base2+"/api/pve/clusters/cl-1/aplinfo/download", csrf2,
		`{"node":"pve1","storage":"local","template":"x"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("readonly apl = %d, want 409", resp.StatusCode)
	}
	entries2, _ := rec2.List(context.Background(), audit.ListFilter{Action: auditActionPVEAplDownload})
	if len(entries2.Entries) != 1 {
		t.Fatalf("只读拒绝应留审计: %d", len(entries2.Entries))
	}
}

// TestPVEUploadVMAContract multipart 流式上传 → 200 {volid} + 审计;缺参/只读分支。
func TestPVEUploadVMAContract(t *testing.T) {
	stub := &stubPVEAdmin{uploadVolid: "/var/lib/vz/dump/test.vma"}
	base, client, csrf, rec := setupPVEAdminAPI(t, stub)
	resp := doMultipart(t, client, base+"/api/pve/clusters/cl-1/upload-vma?node=pve1&storage=local", csrf, "test.vma", "VMA-BYTES")
	raw := readBody(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), "/var/lib/vz/dump/test.vma") {
		t.Fatalf("upload = %d body=%s", resp.StatusCode, raw)
	}
	if stub.uploadName != "test.vma" {
		t.Fatalf("filename 透传: %q", stub.uploadName)
	}
	entries, _ := rec.List(context.Background(), audit.ListFilter{Action: auditActionPVEBackupUpload})
	if len(entries.Entries) != 1 || entries.Entries[0].Detail["filename"] != "test.vma" {
		t.Fatalf("audit: %+v", entries)
	}
	// 缺 node/storage → 400。
	resp = doMultipart(t, client, base+"/api/pve/clusters/cl-1/upload-vma", csrf, "test.vma", "x")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("缺参 = %d, want 400", resp.StatusCode)
	}
	// 只读 → 409 留审计。
	stub2 := &stubPVEAdmin{uploadErr: fmt.Errorf("%w: 只读", pveadmin.ErrReadonly)}
	base2, client2, csrf2, rec2 := setupPVEAdminAPI(t, stub2)
	resp = doMultipart(t, client2, base2+"/api/pve/clusters/cl-1/upload-vma?node=pve1&storage=local", csrf2, "test.vma", "x")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("readonly upload = %d, want 409", resp.StatusCode)
	}
	entries2, _ := rec2.List(context.Background(), audit.ListFilter{Action: auditActionPVEBackupUpload})
	if len(entries2.Entries) != 1 {
		t.Fatalf("只读拒绝应留审计: %d", len(entries2.Entries))
	}
}

// TestPVEUploadBodyTracksLimit 超限错误即使被下游包装,也保留 413 判定依据。
func TestPVEUploadBodyTracksLimit(t *testing.T) {
	body := &pveUploadBody{ReadCloser: http.MaxBytesReader(httptest.NewRecorder(), io.NopCloser(strings.NewReader("12345")), 3)}
	_, err := io.ReadAll(body)
	var maxErr *http.MaxBytesError
	if !errors.As(err, &maxErr) || !body.tooLarge.Load() {
		t.Fatalf("err=%v tooLarge=%v", err, body.tooLarge.Load())
	}
}
