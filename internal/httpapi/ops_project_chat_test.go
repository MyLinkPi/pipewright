package httpapi

// 项目维度 opschat 的 HTTP 层测试:tools?scope= 目录端点 + scope=project 建会话。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/mask"
	"github.com/huangchengsir/pipewright/internal/opschat"
	"github.com/huangchengsir/pipewright/internal/storetest"
	"github.com/huangchengsir/pipewright/internal/vault"
)

type opsProjectBackendStub struct {
	id string
}

func (b opsProjectBackendStub) Snapshot(ctx context.Context, projectID string) (opschat.ProjectSnapshot, error) {
	if projectID != b.id {
		return opschat.ProjectSnapshot{}, opschat.ErrNotFound
	}
	return opschat.ProjectSnapshot{ID: b.id, Name: "demo", RepoURL: "https://git.example/acme/demo.git", DefaultBranch: "main"}, nil
}
func (b opsProjectBackendStub) Info(ctx context.Context, projectID string) (string, error) {
	return `{"name":"demo"}`, nil
}
func (b opsProjectBackendStub) CodeTree(ctx context.Context, projectID, ref, path string) (string, error) {
	return `{"entries":[]}`, nil
}
func (b opsProjectBackendStub) CodeRead(ctx context.Context, projectID, ref, path string, lines int) (string, error) {
	return `{"content":""}`, nil
}
func (b opsProjectBackendStub) PipelineConfig(ctx context.Context, projectID string) (string, error) {
	return `{"yaml":""}`, nil
}
func (b opsProjectBackendStub) RepoConfig(ctx context.Context, projectID, branch string) (string, error) {
	return `{"found":false}`, nil
}
func (b opsProjectBackendStub) Runs(ctx context.Context, projectID, status string, limit int) (string, error) {
	return `{"runs":[]}`, nil
}
func (b opsProjectBackendStub) RunDetail(ctx context.Context, runID string) (string, error) {
	return `{"status":"queued"}`, nil
}
func (b opsProjectBackendStub) RunLogs(ctx context.Context, runID string, stepOrdinal, lines int) (string, error) {
	return "", nil
}
func (b opsProjectBackendStub) TriggerRun(ctx context.Context, projectID, branch string, params map[string]string) (string, error) {
	id := uuid.NewString()
	return `{"runId":"` + id + `","status":"queued"}`, nil
}
func (b opsProjectBackendStub) CancelRun(ctx context.Context, runID string) (string, error) {
	return `{"status":"failed"}`, nil
}

func opsProjectFixture(t *testing.T) (*opschat.Service, *opsAuth, string) {
	t.Helper()
	db := storetest.OpenDB(t)
	v := vault.New(db, &[32]byte{1})
	masker := mask.NewMasker()
	e := &opsExecutor{id: uuid.NewString()}
	pid := uuid.NewString()
	s, err := opschat.New(opschat.Options{DB: db, Vault: v, Executor: e, Model: &opsModelStub{}, Projects: opsProjectBackendStub{id: pid}, Masker: masker, LocalRecorder: audit.New(db, masker, nil)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := s.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return s, &opsAuth{}, pid
}

func TestOpsToolsScopeEndpoint(t *testing.T) {
	s, a, _ := opsProjectFixture(t)
	h := New(nil, a, WithOpsChat(s))
	var server, project []opschat.Tool
	json.Unmarshal(opsHTTP(t, h, "GET", "/api/ai/ops/tools", "", 200).Body.Bytes(), &server)
	json.Unmarshal(opsHTTP(t, h, "GET", "/api/ai/ops/tools?scope=project", "", 200).Body.Bytes(), &project)
	if len(server) != 14 || len(project) != 24 {
		t.Fatalf("catalog sizes: server=%d project=%d", len(server), len(project))
	}
	opsHTTP(t, h, "GET", "/api/ai/ops/tools?scope=bogus", "", 422)
	opsHTTP(t, h, "GET", "/api/ai/ops/tools?extra=1", "", 422)
}

func TestOpsProjectSessionCreate(t *testing.T) {
	s, a, pid := opsProjectFixture(t)
	h := New(nil, a, WithOpsChat(s))
	body := `{"scope":"project","projectId":"` + pid + `","context":"projects"}`
	w := opsHTTP(t, h, "POST", "/api/ai/ops/sessions", body, 201)
	var created opschat.Session
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Scope != opschat.ScopeProject || created.ProjectID != pid || len(created.ServerIDs) != 0 {
		t.Fatalf("project session view: %+v", created)
	}
	// 未知项目 → 400;非法 scope → 400;机器会话不受影响。
	opsHTTP(t, h, "POST", "/api/ai/ops/sessions", `{"scope":"project","projectId":"`+uuid.NewString()+`"}`, 422)
	opsHTTP(t, h, "POST", "/api/ai/ops/sessions", `{"scope":"cluster"}`, 422)
	if _, err := s.Create(context.Background(), opschat.CreateInput{ServerIDs: []string{uuid.NewString()}}); err != nil {
		t.Fatal(err)
	}
	// 项目维度可以追加机器目标(≤7);第 8 台 → 400。
	ids := []string{}
	for i := 0; i < 8; i++ {
		ids = append(ids, uuid.NewString())
	}
	if _, err := s.Create(context.Background(), opschat.CreateInput{Scope: opschat.ScopeProject, ProjectID: pid, ServerIDs: ids[:7]}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(context.Background(), opschat.CreateInput{Scope: opschat.ScopeProject, ProjectID: pid, ServerIDs: ids}); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("expected rejection for 8 servers, got %v", err)
	}
}
