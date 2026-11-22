package httpapi

// pveadmin_upgrade_test.go — 升级面 HTTP 契约:更新矩阵/日常升级/重启/仓库管理/
// 大版本升级端点形状、错误码映射、readonly 拒绝与受理审计。

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/pveadmin"
)

func TestPVEUpgradeEndpoints(t *testing.T) {
	stub := &stubPVEAdmin{
		updatesOV:     &pveadmin.UpdatesOverview{Quorate: true, Items: []pveadmin.NodeUpdates{{Node: "pve1", Online: true, Pending: 2}}},
		refreshItems:  []pveadmin.RefreshItem{{Node: "pve1", UPID: "UPID:pve1:1:aptupdate:0:"}},
		upgradeStatus: pveadmin.UpgradeStatus{Done: true, OK: 1},
	}
	base, client, csrf, rec := setupPVEAdminAPI(t, stub)

	resp := doJSON(t, client, http.MethodGet, base+"/api/pve/clusters/cl-1/updates", csrf, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("updates = %d", resp.StatusCode)
	}
	resp = doJSON(t, client, http.MethodPost, base+"/api/pve/clusters/cl-1/updates/refresh", csrf, `{"nodes":["pve1"]}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("refresh = %d", resp.StatusCode)
	}
	resp = doJSON(t, client, http.MethodPost, base+"/api/pve/clusters/cl-1/upgrade", csrf, `{"nodes":["pve1"]}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("upgrade = %d", resp.StatusCode)
	}
	resp = doJSON(t, client, http.MethodGet, base+"/api/pve/clusters/cl-1/upgrade-status", csrf, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upgrade-status = %d", resp.StatusCode)
	}
	for _, action := range []string{auditActionPVEUpgradeRefresh, auditActionPVEUpgradeStart} {
		entries, err := rec.List(context.Background(), audit.ListFilter{Action: action})
		if err != nil || len(entries.Entries) != 1 {
			t.Fatalf("audit %s = %d / %v, want 1", action, len(entries.Entries), err)
		}
	}
}

func TestPVEUpgradeReadonly409(t *testing.T) {
	stub := &stubPVEAdmin{upgradeErr: fmt.Errorf("%w: 只读", pveadmin.ErrReadonly)}
	base, client, csrf, rec := setupPVEAdminAPI(t, stub)
	resp := doJSON(t, client, http.MethodPost, base+"/api/pve/clusters/cl-1/upgrade", csrf, `{}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("readonly upgrade = %d, want 409", resp.StatusCode)
	}
	entries, _ := rec.List(context.Background(), audit.ListFilter{Action: auditActionPVEUpgradeStart})
	if len(entries.Entries) != 1 {
		t.Fatalf("只读拒绝应留审计: %d", len(entries.Entries))
	}
}

func TestPVENodeRebootContract(t *testing.T) {
	stub := &stubPVEAdmin{rebootRef: &pveadmin.TaskRef{Node: "pve1", UPID: "UPID:pve1:1:srvreload:0:"}}
	base, client, csrf, rec := setupPVEAdminAPI(t, stub)
	resp := doJSON(t, client, http.MethodPost, base+"/api/pve/clusters/cl-1/nodes/pve1/reboot", csrf, `{"confirm":"pve1"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("reboot = %d", resp.StatusCode)
	}
	if stub.rebootConfirm != "pve1" {
		t.Fatalf("confirm 透传 = %q", stub.rebootConfirm)
	}
	entries, _ := rec.List(context.Background(), audit.ListFilter{Action: auditActionPVENodeReboot})
	if len(entries.Entries) != 1 {
		t.Fatalf("reboot 审计 = %d", len(entries.Entries))
	}
}

func TestPVEAptRepoEndpoints(t *testing.T) {
	stub := &stubPVEAdmin{
		aptInv:        &pveadmin.AptRepoInventory{Node: "pve1", Codename: "bookworm"},
		aptOverview:   &pveadmin.AptRepoOverview{Items: []pveadmin.AptRepoNodeSummary{{Node: "pve1", Online: true, PveRepo: "enterprise", Mirror: "official"}}},
		aptRes:        &pveadmin.AptRepoWriteResult{Node: "pve1", OK: true, UpdateOK: true},
		aptApplyItems: []pveadmin.AptRepoApplyItem{{Node: "pve1", OK: true}, {Node: "pve2", OK: true}},
	}
	base, client, csrf, rec := setupPVEAdminAPI(t, stub)

	resp := doJSON(t, client, http.MethodGet, base+"/api/pve/clusters/cl-1/nodes/pve1/apt-repos", csrf, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("apt-repos = %d", resp.StatusCode)
	}
	resp = doJSON(t, client, http.MethodGet, base+"/api/pve/clusters/cl-1/apt-repos/overview", csrf, "")
	rawOv := readBody(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(rawOv), "enterprise") {
		t.Fatalf("overview = %d body=%s", resp.StatusCode, rawOv)
	}
	resp = doJSON(t, client, http.MethodPost, base+"/api/pve/clusters/cl-1/nodes/pve1/apt-repos/toggle", csrf,
		`{"path":"/etc/apt/sources.list","key":"4","enabled":true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("toggle = %d", resp.StatusCode)
	}
	resp = doJSON(t, client, http.MethodPost, base+"/api/pve/clusters/cl-1/nodes/pve1/apt-repos/mirror", csrf, `{"preset":"ustc"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mirror = %d", resp.StatusCode)
	}
	resp = doJSON(t, client, http.MethodPost, base+"/api/pve/clusters/cl-1/apt-repos/apply", csrf, `{"action":"mirror","preset":"ustc"}`)
	raw := readBody(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), "pve2") {
		t.Fatalf("apply = %d body=%s", resp.StatusCode, raw)
	}
	entries, _ := rec.List(context.Background(), audit.ListFilter{Action: auditActionPVEAptToggle})
	if len(entries.Entries) != 1 {
		t.Fatalf("toggle 审计 = %d", len(entries.Entries))
	}
}

func TestPVEMajorUpgradeContracts(t *testing.T) {
	stub := &stubPVEAdmin{
		majorRep:    &pveadmin.MajorPreflightReport{ClusterID: "cl-1", Quorate: true},
		majorStatus: pveadmin.MajorUpgradeStatus{JobID: "j1", Status: pveadmin.MajorStatusRunning, Running: true},
	}
	base, client, csrf, rec := setupPVEAdminAPI(t, stub)

	resp := doJSON(t, client, http.MethodGet, base+"/api/pve/clusters/cl-1/major-upgrade/preflight", csrf, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("preflight = %d", resp.StatusCode)
	}
	resp = doJSON(t, client, http.MethodPost, base+"/api/pve/clusters/cl-1/major-upgrade", csrf,
		`{"confirm":"pve-prod","acks":["self-hosting"],"guestPolicy":"shutdown"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("major start = %d", resp.StatusCode)
	}
	if stub.majorIn.GuestPolicy != "shutdown" {
		t.Fatalf("majorIn = %+v", stub.majorIn)
	}
	resp = doJSON(t, client, http.MethodGet, base+"/api/pve/clusters/cl-1/major-upgrade-status", csrf, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("major status = %d", resp.StatusCode)
	}
	resp = doJSON(t, client, http.MethodPost, base+"/api/pve/clusters/cl-1/major-upgrade/abort", csrf, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("abort = %d", resp.StatusCode)
	}
	resp = doJSON(t, client, http.MethodPost, base+"/api/pve/clusters/cl-1/major-upgrade/retry", csrf, `{"node":"pve1","confirm":"pve-prod"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("retry = %d", resp.StatusCode)
	}
	resp = doJSON(t, client, http.MethodPost, base+"/api/pve/clusters/cl-1/major-upgrade/resume", csrf, `{"confirm":"pve-prod"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("resume = %d", resp.StatusCode)
	}
	for _, action := range []string{auditActionPVEMajorStart, auditActionPVEMajorAbort, auditActionPVEMajorRetry, auditActionPVEMajorResume} {
		entries, _ := rec.List(context.Background(), audit.ListFilter{Action: action})
		if len(entries.Entries) != 1 {
			t.Fatalf("audit %s = %d, want 1", action, len(entries.Entries))
		}
	}
}

func TestPVEUpgradeErrorMapping(t *testing.T) {
	cases := []struct {
		err      error
		wantCode int
		wantErr  string
	}{
		{fmt.Errorf("%w: 须输入集群名", pveadmin.ErrConfirmMismatch), http.StatusUnprocessableEntity, "pve_confirm_mismatch"},
		{fmt.Errorf("%w: 请先预检", pveadmin.ErrPreflightRequired), http.StatusConflict, "pve_preflight_required"},
		{fmt.Errorf("%w: ceph", pveadmin.ErrCephDetected), http.StatusConflict, "pve_ceph_manual"},
		{fmt.Errorf("%w: sudo", pveadmin.ErrNoPrivilege), http.StatusConflict, "pve_no_privilege"},
		{fmt.Errorf("%w: ssh", pveadmin.ErrProbe), http.StatusBadGateway, "pve_ssh_failed"},
	}
	for _, c := range cases {
		stub := &stubPVEAdmin{majorStartErr: c.err}
		base, client, csrf, _ := setupPVEAdminAPI(t, stub)
		resp := doJSON(t, client, http.MethodPost, base+"/api/pve/clusters/cl-1/major-upgrade", csrf, `{"confirm":"x"}`)
		raw := readBody(t, resp)
		if resp.StatusCode != c.wantCode || !strings.Contains(string(raw), c.wantErr) {
			t.Errorf("err %v → %d body=%s, want %d/%s", c.err, resp.StatusCode, raw, c.wantCode, c.wantErr)
		}
	}
}
