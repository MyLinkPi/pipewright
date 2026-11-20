package opschat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestNormalizeJustificationLineEndings(t *testing.T) {
	cases := []struct{ in, want string }{
		{"line1\r\nline2", "line1\nline2"},
		{"a\rb", "a\nb"},
		{"a\r\nb\rc\n", "a\nb\nc\n"},
		{"plain\ttext\n", "plain\ttext\n"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := normalizeJustification(tc.in); got != tc.want {
			t.Fatalf("normalizeJustification(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestValidateAgentJustification(t *testing.T) {
	cjk512 := strings.Repeat("磁", 512) // 512 字符 = 1536 字节:按字符校验必须放行
	cases := []struct {
		name    string
		purpose string
		reason  string
		want    []string // 英文(回灌模型)错误子串;空 = 校验通过
		wantZh  []string // 中文(给用户)文案子串
	}{
		{"ascii", "restart app", "user asked", nil, nil},
		{"cjk", "重启应用", "磁盘占用 81%，需要清理日志释放空间", nil, nil},
		{"multiline-newline-tab", "排查磁盘占用", "证据:\n\t- /var/log 增长\n\t- 镜像层堆积", nil, nil},
		{"exactly-512-chars", strings.Repeat("x", 512), "ok", nil, nil},
		{"cjk-512-runes", cjk512, "ok", nil, nil},
		{"empty-purpose", "", "ok", []string{"purpose is empty"}, []string{"未填写用途"}},
		{"empty-reason", "ok", "", []string{"reason is empty"}, []string{"未填写理由"}},
		{"whitespace-only-purpose", " \n\t ", "ok", []string{"purpose is empty"}, []string{"未填写用途"}},
		{"both-empty", "", "", []string{"purpose is empty", "reason is empty"}, []string{"未填写用途", "未填写理由"}},
		{"oversize-ascii", strings.Repeat("x", 513), "ok", []string{"purpose exceeds 512 characters"}, []string{"用途超过 512 字"}},
		{"oversize-cjk", "ok", strings.Repeat("磁", 513), []string{"reason exceeds 512 characters"}, []string{"理由超过 512 字"}},
		{"escape-control", "ok", "a\x1bb", []string{"reason contains unsupported control characters"}, []string{"理由含非法控制字符"}},
		{"nul-control", "a\x00b", "ok", []string{"purpose contains unsupported control characters"}, []string{"用途含非法控制字符"}},
		{"raw-cr-rejected", "ok", "a\rb", []string{"reason contains unsupported control characters"}, []string{"理由含非法控制字符"}},
		{"invalid-utf8", string([]byte{0xff, 0xfe}), "ok", []string{"purpose is not valid UTF-8"}, []string{"用途编码非法"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAgentJustification("host_exec", tc.purpose, tc.reason)
			if len(tc.want) == 0 {
				if err != nil {
					t.Fatal("unexpected rejection", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected rejection")
			}
			for _, sub := range tc.want {
				if !strings.Contains(err.Error(), sub) {
					t.Fatalf("model-facing error %q missing %q", err, sub)
				}
			}
			user := err.userText()
			if !strings.Contains(user, "host_exec") {
				t.Fatalf("user text missing tool name: %q", user)
			}
			for _, sub := range tc.wantZh {
				if !strings.Contains(user, sub) {
					t.Fatalf("user text %q missing %q", user, sub)
				}
			}
		})
	}
}

// 多行中文理由(旧校验必拒)现在应原样进入确认卡:purpose/reason 不被截断、
// 不改写,审批人看到模型原文。
func TestMultilineCJKJustificationReachesConfirmation(t *testing.T) {
	id := uuid.NewString()
	f := executor(id)
	m := model()
	s, _, _ := fixture(t, f, m)
	c := chat(t, s, id)
	reason := "磁盘占用 81%。\n需要确认大文件归属:\n\t- /var/log 日志\n\t- 容器镜像层"
	m.reply = AgentReply{ToolCalls: []AgentToolCall{{
		ID: uuid.NewString(), ToolID: "docker_action", Target: "target_0",
		Purpose: "重启应用以释放句柄", Reason: reason,
		Args: json.RawMessage(`{"container":"app","action":"restart"}`),
	}}}
	r := submitTool(t, s, c, id)
	snap := waitRun(t, s, c.ID, r.ID, AwaitingConfirmation)
	if len(snap.Confirmations) != 1 {
		t.Fatal("expected one confirmation", snap.Confirmations)
	}
	var bound *BoundCall
	for i := range snap.Confirmations[0].Calls {
		if snap.Confirmations[0].Calls[i].ToolID == "docker_action" {
			bound = &snap.Confirmations[0].Calls[i]
		}
	}
	if bound == nil {
		t.Fatal("docker_action missing from confirmation")
	}
	if bound.Purpose != "重启应用以释放句柄" || bound.Reason != reason {
		t.Fatal("justification altered or truncated", bound.Purpose, bound.Reason)
	}
	// 确认绑定可正常消费:规范化/scrub 后的文本与密封审批体一致。
	if _, err := s.Confirm(context.Background(), c.ID, ConfirmInput{RunID: r.ID, Nonce: snap.Confirmations[0].Nonce, Calls: snap.Confirmations[0].Calls}); err != nil {
		t.Fatal("confirm rejected intact justification", err)
	}
	waitJobs(t, s)
}

// 只读白名单命令(df)免确认亦免理由:省略 purpose/reason 不再 invalid_plan,
// 直接自动放行执行——磁盘排查场景反复报错的根因修复。
func TestReadOnlyExecSkipsJustification(t *testing.T) {
	id := uuid.NewString()
	f := executor(id)
	m := model()
	s, _, _ := fixture(t, f, m)
	c := chat(t, s, id)
	m.reply = AgentReply{ToolCalls: []AgentToolCall{{
		ID: uuid.NewString(), ToolID: "host_exec", Target: "target_0",
		Args: json.RawMessage(`{"argv":["df","-h"]}`), // 无 Purpose/Reason
	}}}
	r := submitTool(t, s, c, id)
	snap := waitRun(t, s, c.ID, r.ID, Succeeded)
	waitJobs(t, s)
	if len(snap.Confirmations) != 0 {
		t.Fatal("read-only exec produced a confirmation card", snap.Confirmations)
	}
	if len(snap.Calls) != 1 || snap.Calls[0].ToolID != "host_exec" || snap.Calls[0].Status != Succeeded {
		t.Fatal("read-only exec did not run without justification", snap.Calls)
	}
}
