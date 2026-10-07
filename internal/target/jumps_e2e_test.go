package target

// jumps_e2e_test.go 是 SSH 跳板链**真传输**的端到端集成测试(与 interactive_e2e_test.go
// 同一门控):用本机 sshd 同时充当跳板与目标 —— 本机 → localhost:22(跳)→ localhost:22(目标),
// 真实验证 direct-tcpip 转发、二次 SSH 握手、会话执行与整链收尾(fake 证不到的真实栈行为)。
//
// 默认 SKIP:仅当 PIPEWRIGHT_E2E_SSH=1 且本机 sshd 可免密连(且允许 TCP 转发,
// OpenSSH 默认 AllowTcpForwarding yes)时运行。CI 无此环境。
// 跑法:PIPEWRIGHT_E2E_SSH=1 go test ./internal/target/ -run E2EMultiHop -race -v

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

const e2eLoopbackSSHAddr = "localhost:22"

func TestE2EMultiHopRealSSH(t *testing.T) {
	if os.Getenv("PIPEWRIGHT_E2E_SSH") != "1" {
		t.Skip("设 PIPEWRIGHT_E2E_SSH=1 启用真 SSH 多跳 e2e")
	}
	key := loadE2EKey(t)
	user := os.Getenv("USER")
	if user == "" {
		t.Skip("USER 为空,跳过")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg := SSHConfig{
		User:       user,
		PrivateKey: key,
		Jumps:      []JumpConfig{{Addr: e2eLoopbackSSHAddr, User: user, PrivateKey: key}},
	}

	res, err := sshDialer{}.Run(ctx, e2eLoopbackSSHAddr, cfg, []string{"echo", "multi-hop-ok"})
	if err != nil {
		t.Fatalf("经跳板 Run: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "multi-hop-ok") {
		t.Fatalf("结果不符: %+v", res)
	}

	// 失败链路:跳板认证材料无效 → 错误带跳位前缀且仍是认证类 sentinel。
	badCfg := SSHConfig{
		User:       user,
		PrivateKey: key,
		Jumps:      []JumpConfig{{Addr: e2eLoopbackSSHAddr, User: user, Password: "definitely-not-the-password"}},
	}
	_, err = sshDialer{}.Run(ctx, e2eLoopbackSSHAddr, badCfg, []string{"echo", "x"})
	if err == nil {
		t.Fatalf("无效跳板凭据应失败")
	}
	if !strings.Contains(err.Error(), "第1跳") {
		t.Fatalf("错误未标注跳位: %v", err)
	}
	if !strings.Contains(humanError(err), "第1跳") {
		t.Fatalf("人读错误未透出跳位: %s", humanError(err))
	}
}
