package easytier

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// fakeCredRunner 记录调用参数并返回预置输出(fake 私钥串故意用真实 base62 长度)。
type fakeCredRunner struct {
	calls   [][]string
	outs    []string // 逐次返回;耗尽后返回最后一条
	err     error
	callIdx int
}

func (f *fakeCredRunner) run(_ context.Context, args []string) ([]byte, error) {
	f.calls = append(f.calls, args)
	if f.err != nil {
		return nil, f.err
	}
	i := f.callIdx
	if i >= len(f.outs) {
		i = len(f.outs) - 1
	}
	f.callIdx++
	return []byte(f.outs[i]), nil
}

const fakeSecret = "kQ7vT2xP9wLmN4bR8cYhU3jF6sD1gZ5a"

// TestCredentialGenerate 验证 generate 拼参与输出解析:JSON 形态与文本形态都能取到
// 私钥;传入预生成 credential_id(--credential-id),ID 不依赖输出。
func TestCredentialGenerate(t *testing.T) {
	cases := []struct {
		name string
		out  string
	}{
		{"JSON snake", `{"credential_id":"abc","credential_secret":"` + fakeSecret + `"}`},
		{"JSON camel", `{"credentialSecret":"` + fakeSecret + `"}`},
		{"文本冒号", "credential_id: abc\ncredential_secret: " + fakeSecret + "\n"},
		{"文本等号", "credential_secret=" + fakeSecret},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeCredRunner{outs: []string{c.out}}
			p := newCredentialProviderForTest("127.0.0.1:15888", f.run)
			secret, err := p.Generate(context.Background(), 86400, "uuid-1")
			if err != nil {
				t.Fatalf("Generate 报错:%v", err)
			}
			if secret != fakeSecret {
				t.Fatalf("私钥解析错:got %q", secret)
			}
			want := []string{"--rpc-portal", "127.0.0.1:15888", "credential", "generate",
				"--ttl", "86400", "--credential-id", "uuid-1"}
			if !reflect.DeepEqual(f.calls[0], want) {
				t.Fatalf("参数错:got %v", f.calls[0])
			}
		})
	}
}

// TestCredentialGenerateFailures 验证:执行失败透传错误;输出无私钥时报错而非猜测;
// 非法入参(ttl≤0/空 ID)直接拒绝不触 CLI。
func TestCredentialGenerateFailures(t *testing.T) {
	p := newCredentialProviderForTest("127.0.0.1:15888", func(_ context.Context, _ []string) ([]byte, error) {
		return nil, errors.New("boom")
	})
	if _, err := p.Generate(context.Background(), 3600, "id"); err == nil {
		t.Fatal("执行失败应报错")
	}

	f := &fakeCredRunner{outs: []string{"credential generated successfully"}}
	p2 := newCredentialProviderForTest("127.0.0.1:15888", f.run)
	if _, err := p2.Generate(context.Background(), 3600, "id"); err == nil {
		t.Fatal("输出无私钥应报错(绝不猜测截取)")
	}

	p3 := newCredentialProviderForTest("127.0.0.1:15888", f.run)
	if _, err := p3.Generate(context.Background(), 0, "id"); err == nil {
		t.Fatal("ttl≤0 应拒绝")
	}
	if len(f.calls) != 1 { // 上一次成功记录 1 次;两次非法入参不应再触 CLI
		t.Fatalf("非法入参不应触 CLI(calls=%d)", len(f.calls))
	}
}

// TestCredentialList 验证 list 解析:JSON 数组 / items 包装 / 文本 UUID 兜底 / 空输出。
func TestCredentialList(t *testing.T) {
	jsonArr := `[{"credential_id":"id-1","expiry_unix":1790000000,"reusable":true,"allow_relay":false,"groups":["credential"]},
	             {"credentialId":"id-2","expiry":"1790000123"}]`
	cases := []struct {
		name string
		out  string
		want []CredentialEntry
	}{
		{"JSON 数组", jsonArr, []CredentialEntry{
			{CredentialID: "id-1", ExpiryUnix: 1790000000, Reusable: true, Groups: []string{"credential"}},
			{CredentialID: "id-2", ExpiryUnix: 1790000123},
		}},
		{"items 包装", `{"items":[{"credential_id":"id-1"}]}`, []CredentialEntry{{CredentialID: "id-1"}}},
		{"文本 UUID 兜底", "credential 0a1b2c3d-4e5f-6789-abcd-ef0123456789 active\nnote: x",
			[]CredentialEntry{{CredentialID: "0a1b2c3d-4e5f-6789-abcd-ef0123456789"}}},
		{"空输出", "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newCredentialProviderForTest("127.0.0.1:15888", func(_ context.Context, _ []string) ([]byte, error) {
				return []byte(c.out), nil
			})
			got, err := p.List(context.Background())
			if err != nil {
				t.Fatalf("List 报错:%v", err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("条数错:got %d want %d (%v)", len(got), len(c.want), got)
			}
			for i := range got {
				if got[i].CredentialID != c.want[i].CredentialID || got[i].ExpiryUnix != c.want[i].ExpiryUnix {
					t.Fatalf("条目 %d 错:got %+v want %+v", i, got[i], c.want[i])
				}
			}
		})
	}
}

// TestCredentialRevoke 验证 revoke 拼参与错误透传。
func TestCredentialRevoke(t *testing.T) {
	f := &fakeCredRunner{outs: []string{"ok"}}
	p := newCredentialProviderForTest("127.0.0.1:15888", f.run)
	if err := p.Revoke(context.Background(), "id-9"); err != nil {
		t.Fatalf("Revoke 报错:%v", err)
	}
	want := []string{"--rpc-portal", "127.0.0.1:15888", "credential", "revoke", "id-9"}
	if !reflect.DeepEqual(f.calls[0], want) {
		t.Fatalf("参数错:got %v", f.calls[0])
	}
	if err := p.Revoke(context.Background(), "  "); err == nil {
		t.Fatal("空 ID 应拒绝")
	}
}

// TestBuildArgsCredential 验证凭据入网传参:凭据 → --credential + 显式 --secure-mode,
// 且不再传 --network-secret(凭据设备不持有组网密钥);两者并存时凭据优先;
// 仅密钥时行为不变(无凭据旗子)。
func TestBuildArgsCredential(t *testing.T) {
	cred := Config{NetworkName: "net", RPCAddr: "127.0.0.1:15888", Hostname: "h1", Credential: "CRED-KEY"}
	got := buildArgs(cred, helpFlags{})
	if !containsPair(got, "--credential", "CRED-KEY") {
		t.Fatalf("凭据应传参:%v", got)
	}
	if !containsPair(got, "--secure-mode", "true") {
		t.Fatalf("凭据隐含 secure mode 应显式传参:%v", got)
	}
	for _, a := range got {
		if a == "--network-secret" {
			t.Fatalf("凭据入网不应传组网密钥:%v", got)
		}
	}

	// 凭据优先:两者并存仍只走凭据;SecureMode 叠加也不重复传 --secure-mode。
	both := cred
	both.NetworkSecret = "sec"
	both.SecureMode = true
	got = buildArgs(both, helpFlags{})
	count := 0
	for _, a := range got {
		switch a {
		case "--network-secret":
			t.Fatalf("凭据优先,不应传密钥:%v", got)
		case "--secure-mode":
			count++
		}
	}
	if count != 1 {
		t.Fatalf("--secure-mode 应恰好一次(得 %d):%v", count, got)
	}

	// 仅密钥:无凭据旗子(既有行为回归)。
	secretOnly := Config{NetworkName: "net", RPCAddr: "127.0.0.1:15888", Hostname: "h1", NetworkSecret: "sec"}
	got = buildArgs(secretOnly, helpFlags{})
	if !containsPair(got, "--network-secret", "sec") {
		t.Fatalf("仅密钥应照常传参:%v", got)
	}
	for _, a := range got {
		if a == "--credential" || a == "--secure-mode" {
			t.Fatalf("仅密钥不应传凭据/secure 旗子:%v", got)
		}
	}
}

// TestBuildArgsSecureMode 验证 Secure Mode 与凭据文件传参:开启时显式 true(布尔带值
// 约定),凭据文件非空才传;关闭时两旗子都缺席。
func TestBuildArgsSecureMode(t *testing.T) {
	base := Config{NetworkName: "net", RPCAddr: "127.0.0.1:15888", Hostname: "h1",
		SecureMode: true, CredentialFile: `C:\et\credentials.json`}
	got := buildArgs(base, helpFlags{})
	if !containsPair(got, "--secure-mode", "true") {
		t.Fatalf("SecureMode 应传显式 true:%v", got)
	}
	if !containsPair(got, "--credential-file", base.CredentialFile) {
		t.Fatalf("凭据文件应传参:%v", got)
	}
	off := base
	off.SecureMode = false
	off.CredentialFile = ""
	got = buildArgs(off, helpFlags{})
	for _, a := range got {
		if strings.HasPrefix(a, "--secure-mode") || strings.HasPrefix(a, "--credential-file") {
			t.Fatalf("关闭时不应传参:%v", got)
		}
	}
}
