package opschat

// 本机(控制节点,__local__)作为 AI 运维会话目标的回归测试。
//
// __local__ 不是登记服务器、无 UUID:validTargets 对它有专门放行(internal/target 的
// LocalHostID),执行经 main.go 装配的 WithLocalHost 包装 executor 落到本机 os/exec。
// 这里用 fakeExecutor(按 id 命中,无 UUID 假设)覆盖:会话创建 / terminal 上下文 /
// Submit 冻结目标 → 白名单工具全链路。

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/huangchengsir/pipewright/internal/target"
)

// __local__ 可作会话目标;重复仍拒绝(去重语义不因放行而放宽)。
func TestCreateAcceptsLocalHostTarget(t *testing.T) {
	s, _, _ := fixture(t, executor(target.LocalHostID), model())
	c, e := s.Create(context.Background(), CreateInput{ServerIDs: []string{target.LocalHostID}})
	if e != nil {
		t.Fatalf("Create([__local__]): %v", e)
	}
	if len(c.ServerIDs) != 1 || c.ServerIDs[0] != target.LocalHostID {
		t.Fatalf("session serverIds = %v, want [__local__]", c.ServerIDs)
	}
	if _, e := s.Create(context.Background(), CreateInput{ServerIDs: []string{target.LocalHostID, target.LocalHostID}}); !errors.Is(e, ErrInvalid) {
		t.Fatalf("Create(duplicate __local__) err = %v, want ErrInvalid", e)
	}
}

// __local__ 与真实 UUID 可混选(同一会话多目标,含本机)。
func TestCreateAcceptsMixedLocalAndUuidTargets(t *testing.T) {
	s, _, _ := fixture(t, executor(uuid.NewString(), target.LocalHostID), model())
	if _, e := s.Create(context.Background(), CreateInput{ServerIDs: []string{uuid.NewString(), target.LocalHostID}}); e != nil {
		t.Fatalf("Create(mixed): %v", e)
	}
}

// terminal:__local__ 上下文本机终端页可用;非法形式仍拒绝(uuid 分支语义不变)。
func TestTerminalContextAcceptsLocalHost(t *testing.T) {
	s, _, _ := fixture(t, executor(target.LocalHostID), model())
	if _, e := s.Create(context.Background(), CreateInput{ServerIDs: []string{target.LocalHostID}, Context: "terminal:" + target.LocalHostID}); e != nil {
		t.Fatalf("Create(terminal:__local__): %v", e)
	}
	if _, e := s.Create(context.Background(), CreateInput{ServerIDs: []string{target.LocalHostID}, Context: "terminal:not-a-uuid"}); !errors.Is(e, ErrInvalid) {
		t.Fatalf("Create(terminal:not-a-uuid) err = %v, want ErrInvalid", e)
	}
}

// 本机会话全链路:Submit 冻结目标快照 → 白名单 host_exec 免确认直接执行 →
// 调用记录/派发 argv 均落在 __local__。
func TestLocalHostToolRunEndToEnd(t *testing.T) {
	m := model()
	f := executor(target.LocalHostID)
	s, _, _ := fixture(t, f, m)
	c := chat(t, s, target.LocalHostID)
	r := submitToolPlan(t, s, m, c, "host_exec", `{"argv":["du","-sh","/opt"]}`)
	snap := waitRun(t, s, c.ID, r.ID, Succeeded)
	waitJobs(t, s)
	if len(snap.Confirmations) != 0 {
		t.Fatal("whitelisted exec on local host produced a confirmation card", snap.Confirmations)
	}
	if len(snap.Calls) != 1 || snap.Calls[0].Status != Succeeded || snap.Calls[0].Output == "" {
		t.Fatal("local host exec did not succeed", snap.Calls)
	}
	if snap.Calls[0].ServerID != target.LocalHostID {
		t.Fatalf("call serverId = %q, want __local__", snap.Calls[0].ServerID)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.invocations) != 1 || f.invocations[0].server != target.LocalHostID {
		t.Fatal("dispatch did not target __local__", f.invocations)
	}
}
