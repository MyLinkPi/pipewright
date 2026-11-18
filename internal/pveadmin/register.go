// register.go — 建机后自动登记为平台服务器(aliyunecs.Import 同形)。
//
// 流程:IP 获取(静态直用 + TCP 探活;DHCP 经 guest agent / lxc interfaces 轮询)→
// 凭据解析(已有 vault 凭据或内联代建,XOR)→ 按 pve_vm_links 幂等匹配(命中更新原
// 机器行,未命中新建 standard 服务器)→ 回写映射。登记失败不否定建机结果(分开计错)。
//
// 纪律:登记的机器 Kind 留空 = standard(可进部署选择器;PVE 物理机 pve_node 被各层
// 拦截,虚机按普通服务器登记是合法路径);凭据明文仅进程内,绝不进日志/审计/错误体。
package pveadmin

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/huangchengsir/pipewright/internal/store"
	"github.com/huangchengsir/pipewright/internal/target"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// RegisterInput 是建机后自动登记参数(凭据二选一:XOR)。
type RegisterInput struct {
	SSHUser      string        `json:"sshUser"`
	SSHPort      int           `json:"sshPort"` // 0 → 22
	CredentialID string        `json:"credentialId"`
	InlineSecret *InlineSecret `json:"inlineSecret,omitempty"`
	WaitSeconds  int           `json:"waitSeconds"` // IP 等待/探活;0 → 180,上限 600
	Labels       string        `json:"labels"`
}

// InlineSecret 是内联 SSH 凭据(代建一条 vault 凭据,整批共用)。
type InlineSecret struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"` // password | key
	Secret string `json:"secret"`
}

func validateRegisterInput(r *RegisterInput) error {
	r.SSHUser = strings.TrimSpace(r.SSHUser)
	if !ciSafeRe.MatchString(r.SSHUser) {
		return fmt.Errorf("%w: 登记 SSH 用户名不合规", ErrInvalidInput)
	}
	if r.SSHPort <= 0 {
		r.SSHPort = target.DefaultPort
	}
	if r.SSHPort > 65535 {
		return fmt.Errorf("%w: 登记 SSH 端口越界", ErrInvalidInput)
	}
	if (r.CredentialID != "") == (r.InlineSecret != nil) {
		return fmt.Errorf("%w: 登记凭据须为已有凭据或内联凭据二选一", ErrInvalidInput)
	}
	if r.InlineSecret != nil {
		switch r.InlineSecret.Kind {
		case "password":
			if r.InlineSecret.Secret == "" || len(r.InlineSecret.Secret) > 4096 {
				return fmt.Errorf("%w: 内联密码为空或超长", ErrInvalidInput)
			}
		case "key":
			if !strings.Contains(r.InlineSecret.Secret, "-----BEGIN") || !strings.Contains(r.InlineSecret.Secret, "PRIVATE KEY") {
				return fmt.Errorf("%w: 内联私钥须为 PEM 格式", ErrInvalidInput)
			}
		default:
			return fmt.Errorf("%w: 内联凭据类型须为 password|key", ErrInvalidInput)
		}
	}
	if r.WaitSeconds <= 0 {
		r.WaitSeconds = 180
	}
	if r.WaitSeconds > 600 {
		r.WaitSeconds = 600
	}
	if len(r.Labels) > 256 {
		return fmt.Errorf("%w: 标签超长", ErrInvalidInput)
	}
	return nil
}

// createInlineCredential 代建一条内联 SSH 凭据;批任务由 createPipeline 只调用一次。
func (s *service) createInlineCredential(secret *InlineSecret, guestName string) (string, error) {
	if s.vault == nil {
		return "", fmt.Errorf("%w: %s", ErrVaultUnconfigured, "master key not configured")
	}
	typ := vault.TypeSSHPassword
	if secret.Kind == "key" {
		typ = vault.TypeSSHKey
	}
	credName := secret.Name
	if credName == "" {
		credName = "pve-" + guestName
	}
	cred, err := s.vault.Create(vault.CreateInput{Name: credName, Type: typ, Secret: secret.Secret})
	if err != nil {
		return "", fmt.Errorf("pveadmin: 代建 SSH 凭据失败: %w", err)
	}
	return cred.ID, nil
}

// registerCreatedGuest 把建好的虚机/容器登记为 standard 服务器,返回服务器 ID。
// 探测到的 IP 回填 item.IP(DHCP 模式建机时未知)。
func (s *service) registerCreatedGuest(ctx context.Context, j *createJob, cl *Cluster, c pveClient, in CreateInput, vmid int64, name, staticIP string, item *CreateItemResult) (string, error) {
	r := in.Register
	wait := time.Duration(r.WaitSeconds) * time.Second

	// 1) IP:静态直用(探活非致命);DHCP 经 agent/interfaces 轮询。
	ip := staticIP
	if ip == "" {
		kind := "qemu"
		if in.Mode == CreateModeLXC {
			kind = "lxc"
		}
		detected, err := s.detectGuestIP(ctx, c, in.Node, kind, vmid, wait)
		if err != nil {
			return "", err
		}
		ip = detected
	} else if !tcpReachable(ctx, ip, r.SSHPort, wait) {
		j.logf("[%s] %s:%d 探活超时,仍按已知 IP 登记", name, ip, r.SSHPort)
	}
	item.IP = ip

	// 2) 凭据:已有或内联代建(明文仅进程内)。
	credID := r.CredentialID
	if r.InlineSecret != nil {
		var err error
		credID, err = s.createInlineCredential(r.InlineSecret, name)
		if err != nil {
			return "", err
		}
	}

	// 3) 幂等匹配:链接命中且机器行仍在 → 更新;否则新建 + 回写映射。
	var existing string
	_ = s.db.QueryRowContext(ctx,
		`SELECT server_id FROM pve_vm_links WHERE cluster_id = ? AND node = ? AND vmid = ?`,
		cl.ID, in.Node, vmid).Scan(&existing)
	if existing != "" {
		if _, gerr := s.servers.Get(ctx, existing); gerr == nil {
			if _, uerr := s.servers.Update(ctx, existing, target.UpdateInput{
				Host: &ip, User: &r.SSHUser, Port: &r.SSHPort, CredentialID: &credID,
			}); uerr != nil {
				return "", fmt.Errorf("pveadmin: 更新已登记服务器失败: %w", uerr)
			}
			return existing, nil
		}
	}
	created, err := s.servers.Create(ctx, target.CreateInput{
		Name: name, Host: ip, Port: r.SSHPort, User: r.SSHUser, CredentialID: credID, Labels: r.Labels,
	})
	if err != nil {
		return "", fmt.Errorf("pveadmin: 登记服务器失败: %w", err)
	}
	if lerr := s.upsertVMLink(ctx, cl.ID, in.Node, vmid, created.ID); lerr != nil {
		// 映射失败不否定登记(机器行已建),仅提示。
		j.logf("[%s] 映射回写失败(服务器已登记): %v", name, lerr)
	}
	return created.ID, nil
}

// upsertVMLink 写/刷新 vmid↔server 映射(双方言 upsert)。
func (s *service) upsertVMLink(ctx context.Context, clusterID, node string, vmid int64, serverID string) error {
	suffix := store.UpsertSuffix(store.DialectOf(s.db),
		[]string{"cluster_id", "node", "vmid"},
		[]string{"server_id", "updated_at"})
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO pve_vm_links (cluster_id, node, vmid, server_id, updated_at)
		 VALUES (?, ?, ?, ?, ?) `+suffix,
		clusterID, node, vmid, serverID, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("pveadmin: upsert vm link: %w", err)
	}
	return nil
}

// ---- IP 探测 ----------------------------------------------------------------------------

// apiAgentNetResp 对应 GET .../qemu/{vmid}/agent/network-get-interfaces。
type apiAgentNetResp struct {
	Result []apiAgentIface `json:"result"`
}

type apiAgentIface struct {
	Name        string       `json:"name"`
	IPAddresses []apiAgentIP `json:"ip-addresses"`
}

type apiAgentIP struct {
	Type   string `json:"ip-address-type"` // ipv4 | ipv6
	IP     string `json:"ip-address"`
	Prefix int    `json:"prefix"`
}

// apiLXCInterface 对应 GET .../lxc/{vmid}/interfaces。
type apiLXCInterface struct {
	Name  string `json:"name"`
	Inet  string `json:"inet"` // 10.0.0.5/24
	Inet6 string `json:"inet6"`
}

// detectGuestIP 轮询 guest agent(qemu)/interfaces(lxc)取首个全局 IPv4(5s 间隔)。
func (s *service) detectGuestIP(ctx context.Context, c pveClient, node, kind string, vmid int64, wait time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for {
		ip := ""
		if kind == "lxc" {
			var ifaces []apiLXCInterface
			if err := c.Get(ctx, fmt.Sprintf("/api2/json/nodes/%s/lxc/%d/interfaces", node, vmid), &ifaces); err == nil {
				for _, iface := range ifaces {
					if iface.Name == "lo" {
						continue
					}
					if addr, err := netip.ParseAddr(strings.SplitN(iface.Inet, "/", 2)[0]); err == nil && usableIPv4(addr) {
						ip = addr.String()
						break
					}
				}
			}
		} else {
			var resp apiAgentNetResp
			if err := c.Get(ctx, fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/agent/network-get-interfaces", node, vmid), &resp); err == nil {
				for _, iface := range resp.Result {
					if iface.Name == "lo" {
						continue
					}
					for _, a := range iface.IPAddresses {
						if a.Type != "ipv4" {
							continue
						}
						if addr, err := netip.ParseAddr(a.IP); err == nil && usableIPv4(addr) {
							ip = addr.String()
							break
						}
					}
					if ip != "" {
						break
					}
				}
			}
		}
		if ip != "" {
			return ip, nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("%w: 未探测到虚机 IP(guest agent 未装/未启动或 DHCP 未就绪)", ErrConnectFailed)
		case <-time.After(5 * time.Second):
		}
	}
}

// usableIPv4 过滤回环/链路本地地址。
func usableIPv4(addr netip.Addr) bool {
	return addr.Is4() && !addr.IsLoopback() && !addr.IsLinkLocalUnicast()
}
