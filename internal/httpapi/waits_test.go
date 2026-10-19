package httpapi

// waits_test.go 覆盖「等待」节点端点:GET /api/runs/{id}/waits(按 run 过滤等待者)与
// POST /api/runs/{id}/skip-wait(投递跳过决定;无等待者 409;缺 jobId 422;未装配 503)。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/waitnode"
)

// waitReqWithRunID 构造带 chi 路由参数 {id} 的请求(handler 经 chi.URLParam 取 run id)。
func waitReqWithRunID(method, target, runID, body string) *http.Request {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", runID)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func TestListRunWaitsHandlerFiltersByRun(t *testing.T) {
	coord := waitnode.New()
	coord.Wait(waitnode.Key("run-1", "j1"), waitnode.WaitInfo{
		Ordinal: 3, JobID: "j1", JobName: "等待上游", StageName: "部署",
		TotalSeconds: 60, Deadline: time.Now().Add(30 * time.Second),
	})
	coord.Wait(waitnode.Key("run-2", "jx"), waitnode.WaitInfo{Ordinal: 0, JobID: "jx"})

	h := makeListRunWaitsHandler(coord)
	rec := httptest.NewRecorder()
	h(rec, waitReqWithRunID(http.MethodGet, "/api/runs/run-1/waits", "run-1", ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Items []runWaitDTO `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Items) != 1 {
		t.Fatalf("items = %d, want 1(只含本 run 等待者)", len(out.Items))
	}
	it := out.Items[0]
	if it.JobID != "j1" || it.Ordinal != 3 || it.JobName != "等待上游" || it.TotalSeconds != 60 {
		t.Fatalf("item 字段不符:%+v", it)
	}
	if it.RemainingSeconds <= 0 || it.RemainingSeconds > 30 {
		t.Fatalf("remainingSeconds = %d,应在 (0,30]", it.RemainingSeconds)
	}
}

func TestSkipWaitHandlerResolvesWaiter(t *testing.T) {
	coord := waitnode.New()
	ch := coord.Wait(waitnode.Key("run-1", "j1"), waitnode.WaitInfo{JobID: "j1"})

	h := makeSkipWaitHandler(coord, nil) // rec 为 nil 时 recordAudit 应安全跳过
	rec := httptest.NewRecorder()
	h(rec, waitReqWithRunID(http.MethodPost, "/api/runs/run-1/skip-wait", "run-1", `{"jobId":"j1"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	select {
	case s := <-ch:
		if s.Actor != "admin" {
			t.Fatalf("actor = %q, want admin", s.Actor)
		}
	default:
		t.Fatal("skip 决定应已投递")
	}
	// 同一等待只决一次:再次 POST → 409。
	rec2 := httptest.NewRecorder()
	h(rec2, waitReqWithRunID(http.MethodPost, "/api/runs/run-1/skip-wait", "run-1", `{"jobId":"j1"}`))
	if rec2.Code != http.StatusConflict {
		t.Fatalf("二次跳过 want 409, got %d", rec2.Code)
	}
}

func TestSkipWaitHandlerValidation(t *testing.T) {
	coord := waitnode.New()
	h := makeSkipWaitHandler(coord, nil)

	rec := httptest.NewRecorder()
	h(rec, waitReqWithRunID(http.MethodPost, "/api/runs/run-1/skip-wait", "run-1", `{"jobId":"  "}`))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("空 jobId want 422, got %d", rec.Code)
	}

	rec2 := httptest.NewRecorder()
	h(rec2, waitReqWithRunID(http.MethodPost, "/api/runs/run-1/skip-wait", "run-1", `{"jobId":"missing"}`))
	if rec2.Code != http.StatusConflict {
		t.Fatalf("无等待者 want 409, got %d", rec2.Code)
	}
}

func TestWaitHandlersUninitializedReturn503(t *testing.T) {
	rec := httptest.NewRecorder()
	makeListRunWaitsHandler(nil)(rec, waitReqWithRunID(http.MethodGet, "/api/runs/r/waits", "r", ""))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("list(nil coord) want 503, got %d", rec.Code)
	}
	rec2 := httptest.NewRecorder()
	makeSkipWaitHandler(nil, nil)(rec2, waitReqWithRunID(http.MethodPost, "/api/runs/r/skip-wait", "r", `{"jobId":"j"}`))
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("skip(nil coord) want 503, got %d", rec2.Code)
	}
}
