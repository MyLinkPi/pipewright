package opschat

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestReadOnlyExecArgvWhitelist(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want bool
	}{
		// 白名单内的常规只读形态。
		{"du", []string{"du", "-sh", "/opt"}, true},
		{"du glob", []string{"du", "-sh", "/var/log/*"}, true},
		{"ps format", []string{"ps", "-eo", "pid,comm,pcpu", "--sort=-pcpu"}, true},
		{"grep pattern", []string{"grep", "-i", "error*", "app.log"}, true},
		{"date iso", []string{"date", "+%Y-%m-%dT%H:%M:%S"}, true},
		{"tail lines", []string{"tail", "-n", "200", "/var/log/syslog"}, true},
		{"md5sum check", []string{"md5sum", "--check", "sums.txt"}, true},
		{"whoami bare", []string{"whoami"}, true},
		{"ss filter port", []string{"ss", "-lntp"}, true},
		// 解释器与包装器:程序不在白名单,后续 token 再无害也不放行。
		{"shell concat", []string{"sh", "-c", "du -sh /opt; rm -rf /"}, false},
		{"bash concat", []string{"bash", "-c", "cat /etc/passwd && touch /tmp/x"}, false},
		{"env wrapper", []string{"env", "rm", "-rf", "/"}, false},
		{"timeout wrapper", []string{"timeout", "10", "rm", "-rf", "/"}, false},
		{"xargs wrapper", []string{"xargs", "rm"}, false},
		{"find exec", []string{"find", "/", "-exec", "rm", "{}", ";"}, false},
		{"awk system", []string{"awk", "system(\"reboot\")"}, false},
		{"docker cli", []string{"docker", "ps"}, false},
		{"systemctl", []string{"systemctl", "status", "nginx"}, false},
		{"journalctl vacuum sibling", []string{"journalctl", "-u", "nginx"}, false},
		{"tee write", []string{"tee", "/etc/passwd"}, false},
		{"truncate", []string{"truncate", "-s", "0", "app.log"}, false},
		// 拼接与元字符:即使 argv[0] 在白名单,任何元字符/空白/引号都回退确认。
		{"semicolon token", []string{"du", "-sh", "/opt;", "rm", "-rf", "/"}, false},
		{"concat in one token", []string{"du", "-sh", "/opt && rm -rf /"}, false},
		{"command substitution", []string{"du", "/opt$(rm -rf /)"}, false},
		{"backtick", []string{"du", "/opt`id`"}, false},
		{"pipe token", []string{"ls", "|wc"}, false},
		{"redirect", []string{"cat", "<,/etc/passwd"}, false},
		{"space in token", []string{"cat", "file with space"}, false},
		{"quote in token", []string{"cat", "-n", "a'b"}, false},
		{"dollar var", []string{"echo", "$HOME"}, false},
		{"newline token", []string{"du", "/opt\nrm"}, false},
		// argv[0] 必须是裸命令名:路径形态(包括伪装路径)一律走确认。
		{"absolute path", []string{"/usr/bin/du", "-sh", "/opt"}, false},
		{"disguised path", []string{"./du"}, false},
		{"evil path", []string{"/tmp/evil/du"}, false},
		// 白名单程序的变体副作用。
		{"date set clock", []string{"date", "-s", "@0"}, false},
		{"date set long", []string{"date", "--set", "2020-01-01"}, false},
		{"date set eq", []string{"date", "--set=2020-01-01"}, false},
		{"date set glued", []string{"date", "-s@0"}, false},
		{"date set cluster", []string{"date", "-us", "2020-01-01"}, false},
		{"date utc ok", []string{"date", "-u"}, true},
		// 结构限制。
		{"empty", nil, false},
		{"too many tokens", append([]string{"du"}, make([]string, 16)...), false},
		{"oversized token", []string{"du", strings.Repeat("a", 2049)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := readOnlyExecArgv(tc.argv); got != tc.want {
				t.Fatalf("readOnlyExecArgv(%q) = %v, want %v", tc.argv, got, tc.want)
			}
		})
	}
}

func TestNeedsApprovalWhitelistGate(t *testing.T) {
	exec := func(id string, argv []string) Call {
		return Call{ToolID: id, Args: ToolArgs{Argv: argv, Container: "web"}}
	}
	if needsApproval(exec("host_exec", []string{"du", "-sh", "/opt"})) {
		t.Fatal("whitelisted host_exec must not need approval")
	}
	if needsApproval(exec("docker_exec", []string{"du", "-sh", "/opt"})) {
		t.Fatal("whitelisted docker_exec must not need approval")
	}
	if !needsApproval(exec("host_exec", []string{"sh", "-c", "du"})) {
		t.Fatal("non-whitelisted host_exec must need approval")
	}
	if !needsApproval(exec("docker_exec", []string{"rm", "-rf", "/tmp/x"})) {
		t.Fatal("non-whitelisted docker_exec must need approval")
	}
	if !needsApproval(Call{ToolID: "systemd_action", Args: ToolArgs{Unit: "nginx.service"}}) {
		t.Fatal("service actions always need approval")
	}
	if needsApproval(Call{ToolID: "host_processes"}) {
		t.Fatal("read-only tools never need approval")
	}
}

// 白名单 host_exec 全程不产生确认卡、不留审批单、不写变更意图审计,直接执行。
func TestWhitelistedHostExecRunsWithoutConfirmation(t *testing.T) {
	m := model()
	id := uuid.NewString()
	f := executor(id)
	s, _, db := fixture(t, f, m)
	c := chat(t, s, id)
	r := submitToolPlan(t, s, m, c, "host_exec", `{"argv":["du","-sh","/opt"]}`)
	snap := waitRun(t, s, c.ID, r.ID, Succeeded)
	waitJobs(t, s)
	if len(snap.Confirmations) != 0 {
		t.Fatal("whitelisted exec produced a confirmation card", snap.Confirmations)
	}
	var approvals int
	if err := db.QueryRow("SELECT COUNT(*) FROM ops_chat_approvals").Scan(&approvals); err != nil || approvals != 0 {
		t.Fatal("whitelisted exec issued an approval record", approvals, err)
	}
	var intents int
	if err := db.QueryRow("SELECT COUNT(*) FROM audit_log WHERE action='ops_chat_intent'").Scan(&intents); err != nil || intents != 0 {
		t.Fatal("whitelisted exec wrote a mutation intent audit", intents, err)
	}
	if snap.Calls[0].Status != Succeeded || snap.Calls[0].Output == "" {
		t.Fatal("whitelisted exec did not succeed", snap.Calls[0])
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.invocations) != 1 || strings.Join(f.invocations[0].argv, " ") != "du -sh /opt" {
		t.Fatal("dispatched argv mismatch", f.invocations)
	}
}

// 白名单 docker_exec 同样免确认;容器按不可变 ID 执行,内层 argv 原样下发。
func TestWhitelistedDockerExecRunsWithoutConfirmation(t *testing.T) {
	m := model()
	id := uuid.NewString()
	f := executor(id)
	s, _, _ := fixture(t, f, m)
	c := chat(t, s, id)
	container := strings.Repeat("a", 64)
	r := submitToolPlan(t, s, m, c, "docker_exec", `{"container":"`+container+`","argv":["du","-sh","/opt"]}`)
	snap := waitRun(t, s, c.ID, r.ID, Succeeded)
	waitJobs(t, s)
	if len(snap.Confirmations) != 0 {
		t.Fatal("whitelisted docker_exec produced a confirmation card", snap.Confirmations)
	}
	if snap.Calls[0].Status != Succeeded {
		t.Fatal("whitelisted docker_exec did not succeed", snap.Calls[0])
	}
	want := []string{"docker", "exec", container, "du", "-sh", "/opt"}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.invocations) != 1 {
		t.Fatal("unexpected invocation count", f.invocations)
	}
	for i, token := range want {
		if f.invocations[0].argv[i] != token {
			t.Fatalf("dispatched argv mismatch: %q", f.invocations[0].argv)
		}
	}
}

// 各种拼接/包装/路径伪装绕过都必须回退到人工确认,绝不自动执行。
func TestExecBypassAttemptsFallBackToConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name string
		args string
	}{
		{"shell concatenation", `{"argv":["sh","-c","du -sh /opt; rm -rf /opt/data"]}`},
		{"metacharacter token", `{"argv":["du","-sh","/opt; rm -rf /opt/data"]}`},
		{"wrapper program", `{"argv":["env","rm","-rf","/opt/data"]}`},
		{"disguised path", `{"argv":["/tmp/evil/du","-sh","/opt"]}`},
		{"mutating variant", `{"argv":["date","-s","@0"]}`},
		{"plain mutation", `{"argv":["rm","-rf","/opt/data"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := model()
			id := uuid.NewString()
			f := executor(id)
			s, _, _ := fixture(t, f, m)
			c := chat(t, s, id)
			r := submitToolPlan(t, s, m, c, "host_exec", tc.args)
			waitRun(t, s, c.ID, r.ID, AwaitingConfirmation)
			waitJobs(t, s)
			f.mu.Lock()
			dispatched := len(f.invocations)
			f.mu.Unlock()
			if dispatched != 0 {
				t.Fatal("bypass attempt was dispatched without approval", dispatched)
			}
		})
	}
}

// 混排批次:白名单命令照常执行,同批非白名单命令挂起确认卡时,
// 白名单命令不得越过确认屏障抢先执行(保持既有串行语义)。
func TestWhitelistedExecRespectsConfirmationBarrier(t *testing.T) {
	m := model()
	id := uuid.NewString()
	f := executor(id)
	s, _, _ := fixture(t, f, m)
	c := chat(t, s, id)
	mutating := AgentToolCall{ID: uuid.NewString(), ToolID: "host_exec", Target: "target_0",
		Args: []byte(`{"argv":["rm","-rf","/opt/cache"]}`), Purpose: "p", Reason: "r"}
	readOnly := AgentToolCall{ID: uuid.NewString(), ToolID: "host_exec", Target: "target_0",
		Args: []byte(`{"argv":["du","-sh","/opt"]}`), Purpose: "p", Reason: "r"}
	m.mu.Lock()
	m.queue = append(m.queue, AgentReply{ToolCalls: []AgentToolCall{mutating, readOnly}})
	m.mu.Unlock()
	r, e := s.Submit(context.Background(), c.ID, TurnInput{ClientRequestID: uuid.NewString(), Revision: c.Revision, Text: "mixed batch"})
	if e != nil {
		t.Fatal(e)
	}
	waitRun(t, s, c.ID, r.ID, AwaitingConfirmation)
	waitJobs(t, s)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, call := range f.invocations {
		if strings.Join(call.argv, " ") == "du -sh /opt" {
			t.Fatal("whitelisted exec crossed the confirmation barrier", call.argv)
		}
	}
}
