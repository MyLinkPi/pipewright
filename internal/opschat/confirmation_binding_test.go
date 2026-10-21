package opschat

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

// 续轮新提议的 mutation 不得被同 run 旧消费确认单放行:应挂新确认卡。
func TestFollowUpMutationRequiresFreshConfirmation(t *testing.T) {
	id := uuid.NewString()
	f := executor(id)
	steps := []AgentReply{
		{ToolCalls: actionsToCalls([]Action{{ToolID: "systemd_action", Args: json.RawMessage(`{"unit":"a.service","action":"restart"}`), TargetIndexes: []int{0}}})},
		{ToolCalls: actionsToCalls([]Action{{ToolID: "systemd_action", Args: json.RawMessage(`{"unit":"b.service","action":"restart"}`), TargetIndexes: []int{0}}})},
	}
	m := &continuationModel{fakeModel: model(), steps: steps}
	s, _, _ := fixture(t, f, m)
	c := chat(t, s, id)
	// 第一轮:系统提示模型提第一批 mutation(走 queue 注入计划)
	m.mu.Lock()
	m.queue = append(m.queue, steps[0], steps[1])
	m.mu.Unlock()
	first, err := s.Submit(context.Background(), c.ID, TurnInput{ClientRequestID: uuid.NewString(), Revision: c.Revision, Text: "restart a then b"})
	if err != nil {
		t.Fatal(err)
	}
	snap := waitRun(t, s, c.ID, first.ID, AwaitingConfirmation)
	a := snap.Confirmations[0]
	if _, err := s.Confirm(context.Background(), c.ID, ConfirmInput{RunID: first.ID, Nonce: a.Nonce, Calls: a.Calls}); err != nil {
		t.Fatal(err)
	}
	// 确认后执行第一批 → 续轮提出第二批 → 应再挂起等新确认,而非 dispatch blocked
	final := waitRun(t, s, c.ID, first.ID, AwaitingConfirmation, Failed)
	if final.Runs[0].Status != AwaitingConfirmation {
		t.Fatalf("second mutation batch should await a fresh confirmation, got %s", final.Runs[0].Status)
	}
	for _, call := range final.Calls {
		if call.ToolID == "systemd_action" && call.Status == Interrupted {
			t.Fatalf("follow-up mutation interrupted by stale approval: %+v", call)
		}
	}
}
