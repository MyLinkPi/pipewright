// credential.go 是 EasyTier 临时凭据的签发/查询/撤销:exec easytier-cli 的 credential
// 子命令,经本机 RPC portal(镜像 status.go 的 cliRunner 注入模式,不碰真机可单测)。
//
// 关键取舍:generate 传入调用方预生成的 --credential-id(UUID)—— 官方参数,且"已存在
// 则返回既有凭据"的语义天然幂等;凭据 ID 因此不依赖输出解析。输出仅需提取
// credential_secret(私钥,临时设备 --credential 入网的值):CLI 输出格式无文档保证,
// JSON / 文本双形态容错解析,真机首验时按实际格式收敛(见 specs/easytier-capability-audit.md)。
package easytier

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// credentialTimeout 是单次 credential 子命令的整体超时(RPC 往返 + 签发运算)。
const credentialTimeout = 10 * time.Second

// CredentialEntry 是一条活跃凭据的快照(list 解析;字段以取到为准,缺失为零值/空)。
type CredentialEntry struct {
	CredentialID string
	ExpiryUnix   int64 // 过期 Unix 秒(0 = 输出未携带/解析失败)
	Reusable     bool
	AllowRelay   bool
	Groups       []string
}

// CredentialIssuer 抽象凭据签发能力(本包 CredentialProvider 原样满足;上层装配
// 与测试经接口引用,便于替换/注入 fake)。
type CredentialIssuer interface {
	Generate(ctx context.Context, ttlSeconds int64, credentialID string) (string, error)
	List(ctx context.Context) ([]CredentialEntry, error)
	Revoke(ctx context.Context, credentialID string) error
}

// CredentialProvider 签发/查询/撤销临时凭据(低频管理操作,每次独立 exec,不做缓存)。
type CredentialProvider struct {
	cliPath string
	rpcAddr string
	run     cliRunner
}

// NewCredentialProvider 构造签发器(cliPath 为 easytier-cli 路径;rpcAddr 为本机 portal)。
func NewCredentialProvider(cliPath, rpcAddr string) *CredentialProvider {
	return &CredentialProvider{cliPath: cliPath, rpcAddr: rpcAddr, run: execCLIRunner(cliPath)}
}

// newCredentialProviderForTest 注入 fake runner 的构造(仅测试用)。
func newCredentialProviderForTest(rpcAddr string, run cliRunner) *CredentialProvider {
	return &CredentialProvider{rpcAddr: rpcAddr, run: run}
}

// Generate 签发一条临时凭据:credentialID 由调用方预生成(UUID,重复签发幂等返回既有
// 凭据),ttlSeconds 为有效期秒。返回 credential_secret —— 仅此一次可见,调用方负责
// 送达设备且绝不落库/落日志。
func (p *CredentialProvider) Generate(ctx context.Context, ttlSeconds int64, credentialID string) (string, error) {
	if ttlSeconds <= 0 || strings.TrimSpace(credentialID) == "" {
		return "", fmt.Errorf("easytier: credential generate 需要正 ttl 与凭据 ID")
	}
	ctx, cancel := context.WithTimeout(ctx, credentialTimeout)
	defer cancel()
	out, err := p.run(ctx, []string{"--rpc-portal", p.rpcAddr, "credential", "generate",
		"--ttl", strconv.FormatInt(ttlSeconds, 10), "--credential-id", credentialID})
	if err != nil {
		return "", err
	}
	return parseCredentialSecret(out)
}

// List 返回全部活跃凭据(官方语义"List all active credentials":过期/已撤销不在内)。
func (p *CredentialProvider) List(ctx context.Context) ([]CredentialEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, credentialTimeout)
	defer cancel()
	out, err := p.run(ctx, []string{"--rpc-portal", p.rpcAddr, "credential", "list"})
	if err != nil {
		return nil, err
	}
	return parseCredentialList(out), nil
}

// Revoke 撤销一条凭据(撤销随路由同步全网生效,约 1~3 秒;使用该凭据的在线节点被踢下线)。
func (p *CredentialProvider) Revoke(ctx context.Context, credentialID string) error {
	if strings.TrimSpace(credentialID) == "" {
		return fmt.Errorf("easytier: credential revoke 需要凭据 ID")
	}
	ctx, cancel := context.WithTimeout(ctx, credentialTimeout)
	defer cancel()
	_, err := p.run(ctx, []string{"--rpc-portal", p.rpcAddr, "credential", "revoke", credentialID})
	return err
}

// ---- 输出解析(JSON 优先,文本兜底;格式未经官方文档承诺) ----

// secretKeys 是 credential_secret 的候选键名(按优先序;覆盖 snake/camel 与泛化写法)。
var secretKeys = []string{"credential_secret", "credentialSecret", "credential-secret", "secret", "private_key", "privateKey"}

// parseCredentialSecret 从 generate 输出提取凭据私钥:整段 JSON 对象按键名取值 →
// 逐行 "key: value"/"key=value" 扫描。取不到返回错误(绝不猜测截取 —— 私钥给错
// 比报错糟糕得多)。
func parseCredentialSecret(out []byte) (string, error) {
	if v := pickJSONString(out, secretKeys); v != "" {
		return v, nil
	}
	for _, line := range strings.Split(string(out), "\n") {
		lower := strings.ToLower(line)
		for _, key := range secretKeys {
			i := strings.Index(lower, key)
			if i < 0 {
				continue
			}
			rest := strings.TrimSpace(line[i+len(key):])
			rest = strings.TrimLeft(rest, ":：= \t")
			// base64 私钥不含空白;取首个空白前的整段。
			if v := strings.Fields(rest); len(v) > 0 && len(v[0]) >= 16 {
				return v[0], nil
			}
		}
	}
	return "", fmt.Errorf("easytier: credential generate 输出未识别出凭据私钥(格式未知,%d 字节;请核对该版本 CLI 输出)", len(out))
}

// parseCredentialList 解析 list 输出:JSON 数组 / {items|credentials:[...]} 对象 →
// 文本兜底(仅提取 UUID 形态的凭据 ID,其余字段留空)。解析宽容:坏条目跳过不整体失败。
func parseCredentialList(out []byte) []CredentialEntry {
	if entries := parseCredentialListJSON(out); len(entries) > 0 {
		return entries
	}
	var entries []CredentialEntry
	for _, token := range strings.Fields(string(out)) {
		token = strings.Trim(token, ",;\"'")
		if isUUIDLike(token) {
			entries = append(entries, CredentialEntry{CredentialID: token})
		}
	}
	return entries
}

// parseCredentialListJSON 尝试把整段输出按 JSON 解析(数组或包一层 items/credentials)。
func parseCredentialListJSON(out []byte) []CredentialEntry {
	var arr []map[string]any
	if err := json.Unmarshal(out, &arr); err != nil {
		var wrap struct {
			Items       []map[string]any `json:"items"`
			Credentials []map[string]any `json:"credentials"`
		}
		if werr := json.Unmarshal(out, &wrap); werr != nil {
			return nil
		}
		arr = append(wrap.Items, wrap.Credentials...)
	}
	entries := make([]CredentialEntry, 0, len(arr))
	for _, obj := range arr {
		e := CredentialEntry{
			CredentialID: firstNonEmptyString(obj, "credential_id", "credentialId", "id"),
			ExpiryUnix:   firstNonEmptyInt(obj, "expiry_unix", "expiryUnix", "expiry", "expires_at"),
			Reusable:     firstNonEmptyBool(obj, "reusable"),
			AllowRelay:   firstNonEmptyBool(obj, "allow_relay", "allowRelay"),
		}
		if groups, ok := obj["groups"].([]any); ok {
			for _, g := range groups {
				if s, ok := g.(string); ok {
					e.Groups = append(e.Groups, s)
				}
			}
		}
		if e.CredentialID != "" {
			entries = append(entries, e)
		}
	}
	return entries
}

// pickJSONString 把 out 按 JSON 对象解析,按键名优先序取第一个非空字符串值。
func pickJSONString(out []byte, keys []string) string {
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		return ""
	}
	for _, k := range keys {
		if v, ok := obj[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func firstNonEmptyString(obj map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := obj[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func firstNonEmptyInt(obj map[string]any, keys ...string) int64 {
	for _, k := range keys {
		switch v := obj[k].(type) {
		case float64:
			return int64(v)
		case string:
			if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
				return n
			}
		}
	}
	return 0
}

func firstNonEmptyBool(obj map[string]any, keys ...string) bool {
	for _, k := range keys {
		if v, ok := obj[k].(bool); ok {
			return v
		}
	}
	return false
}

// isUUIDLike 判定 8-4-4-4-12 十六进制形态(标准 UUID 凭据 ID 的兜底识别)。
func isUUIDLike(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}
