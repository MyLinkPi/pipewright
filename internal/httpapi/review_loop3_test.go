package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/huangchengsir/pipewright/internal/opschat"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// Exercise SSE directly through the handler, without opening any network socket.
type opsFlushRecorder struct {
	*httptest.ResponseRecorder
	flushed chan struct{}
}

func (w *opsFlushRecorder) Flush() {
	w.ResponseRecorder.Flush()
	select {
	case w.flushed <- struct{}{}:
	default:
	}
}

func opsRecordedStream(t *testing.T, h http.Handler, path string) (*opsFlushRecorder, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	r := httptest.NewRequest("GET", path, nil).WithContext(ctx)
	r.AddCookie(&http.Cookie{Name: cookieSession, Value: "valid"})
	w := &opsFlushRecorder{httptest.NewRecorder(), make(chan struct{}, 1)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(w, r)
	}()
	select {
	case <-w.flushed:
	case <-done:
		t.Fatal("stream exited before ready", w.Body.String())
	case <-ctx.Done():
		t.Fatal("stream did not become ready")
	}
	return w, cancel, done
}

func TestOpsHistoricalRotationProjectsHTTPAndSSEWithoutInference(t *testing.T) {
	s, db, e, a, m := opsFixture(t)
	v := vault.New(db, &[32]byte{1})
	secret := "advice"
	text := "historical " + secret
	c, err := s.Create(context.Background(), opschat.CreateInput{Title: text, ServerIDs: []string{e.id}})
	if err != nil {
		t.Fatal(err)
	}
	if c, err = s.Patch(context.Background(), c.ID, opschat.PatchInput{Revision: c.Revision, Draft: &text}); err != nil {
		t.Fatal(err)
	}
	r, err := s.Submit(context.Background(), c.ID, opschat.TurnInput{ClientRequestID: uuid.NewString(), Revision: c.Revision, Text: text})
	if err != nil {
		t.Fatal(err)
	}
	opsWait(t, s, c.ID, r.ID, opschat.Succeeded)
	before := m.calls.Load()
	if _, err = v.Create(vault.CreateInput{Name: "new credential", Type: vault.TypeSSHPassword, Secret: secret}); err != nil {
		t.Fatal(err)
	}
	h := New(nil, a, WithOpsChat(s))
	base := "/api/ai/ops/sessions/" + c.ID
	for _, path := range []string{"/api/ai/ops/sessions", base, base + "/entries"} {
		w := opsHTTP(t, h, "GET", path, "", 200)
		if strings.Contains(w.Body.String(), secret) || !strings.Contains(w.Body.String(), "[MASKED]") {
			t.Fatal("HTTP echoed a newly registered secret", path, w.Body.String())
		}
	}
	w, cancel, done := opsRecordedStream(t, h, base+"/events")
	cancel()
	<-done
	if strings.Contains(w.Body.String(), secret) || !strings.Contains(w.Body.String(), "[MASKED]") || !strings.Contains(w.Body.String(), `"kind":"assistant"`) {
		t.Fatal("SSE echoed historical secret or omitted messages", w.Body.String())
	}
	patched := opsHTTP(t, h, "PATCH", base, fmt.Sprintf(`{"revision":%d,"title":"renamed"}`, c.Revision), 200)
	if strings.Contains(patched.Body.String(), secret) {
		t.Fatal("PATCH echoed historical draft secret", patched.Body.String())
	}
	if e.calls.Load() != 1 || m.calls.Load() != before {
		t.Fatal("public projection dispatched SSH/model inference")
	}
}
