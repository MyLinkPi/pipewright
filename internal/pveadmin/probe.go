// probe.go — PVE 节点探测与集群发现(登记钩子用)。
//
// 探测链(全部固定 argv,绝不拼 shell;命令不含凭据):
//  1. pveversion(免提权,普通用户可跑)exit 0 → 该机是 PVE 节点;
//  2. cat /etc/pve/corosync.conf(经 Runner 提权:root 直跑 / 免密 sudo -n):
//     解析 cluster_name + nodelist(name/ring0_addr)→ 集群成员;
//  3. cat 不可用(无提权)→ 单机 PVE:集群名取 hostname -s,节点 = 自身;
//     cat 报错但非「文件不存在」(pmxcfs 异常等)→ pvecm nodes 兜底解析(地址列)。
//
// Runner 提权语义与 ceph.exec 同形:非 root 且无免密 sudo → ErrNoPrivilege(带 sudoers 指引);
// 非零退出 → ErrExec(带 stderr/stdout 尾部)。单测注入 fake Runner,不触网。
package pveadmin

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/huangchengsir/pipewright/internal/target"
)

// probeTimeout 是单条探测命令的时限(探测在服务器登记响应路径上,快速失败)。
const probeTimeout = 15 * time.Second

// Runner 抽象「对指定登记服务器提权执行 argv 命令」(注入 fake 可离线单测)。
type Runner interface {
	Run(ctx context.Context, serverID string, cmd []string) (*target.ExecResult, error)
}

// NodeInfo 是集群发现得到的一个节点(短主机名 + corosync ring0 地址)。
type NodeInfo struct {
	Name string
	Addr string
}

// PVEInfo 是一次探测的结果:Standalone=单机 PVE(无 corosync),否则 Nodes 为全集群节点;
// SelfName 是该机短主机名(识别 nodelist 里的自身)。
type PVEInfo struct {
	ClusterName string
	Standalone  bool
	SelfName    string
	Nodes       []NodeInfo
}

// sshRunner 是生产 Runner:target.Service SSH 执行 + root/免密 sudo 提权(ceph.exec 同形)。
type sshRunner struct {
	servers target.Service
}

// runCtx 给探测命令套统一超时(探测在登记路径上,不拖长响应)。
func runCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, probeTimeout)
}

// Run 提权执行 cmd:root → 直跑;否则 sudo -n true 探测免密 sudo 通过后前缀 sudo -n。
func (r *sshRunner) Run(ctx context.Context, serverID string, cmd []string) (*target.ExecResult, error) {
	if len(cmd) == 0 {
		return nil, fmt.Errorf("%w: 空命令", ErrInvalidInput)
	}
	sv, err := r.servers.Get(ctx, serverID)
	if err != nil {
		return nil, fmt.Errorf("%w: 服务器 %s 不可用: %v", ErrProbe, serverID, err)
	}
	var full []string
	if sv.User == "root" {
		full = cmd
	} else {
		if res, perr := r.servers.Exec(ctx, serverID, []string{"sudo", "-n", "true"}); perr != nil || res.ExitCode != 0 {
			return nil, fmt.Errorf("%w: 服务器登录用户 %s 非 root 且无免密 sudo,无法读取集群配置;请改用 root 登录或配置免密 sudo(如 /etc/sudoers.d/pipewright 写入「%s ALL=(ALL) NOPASSWD: ALL」)", ErrNoPrivilege, sv.User, sv.User)
		}
		full = append([]string{"sudo", "-n"}, cmd...)
	}
	res, err := r.servers.Exec(ctx, serverID, full)
	if err != nil {
		return nil, fmt.Errorf("%w: %s %s: %v", ErrProbe, sv.Name, cmd[0], err)
	}
	if res.ExitCode != 0 {
		return res, fmt.Errorf("%w: %s 退出码 %d\n%s", ErrProbe, cmd[0], res.ExitCode, tailOutput(res))
	}
	return res, nil
}

// tailOutput 拼装 stderr/stdout 尾部(每段 ≤1000 字符;远端输出不含凭据)。
func tailOutput(res *target.ExecResult) string {
	tail := func(s string) string {
		s = strings.TrimRight(s, "\n")
		if len(s) > 1000 {
			s = "…" + s[len(s)-1000:]
		}
		return s
	}
	var b strings.Builder
	if t := tail(res.Stderr); t != "" {
		b.WriteString("stderr: " + t + "\n")
	}
	if t := tail(res.Stdout); t != "" {
		b.WriteString("stdout: " + t)
	}
	return strings.TrimRight(b.String(), "\n")
}

// probeServer 探测一台登记服务器:非 PVE → (nil, nil)(尽力而为,不视为错误);
// SSH 不可达等执行错误 → (nil, nil) 且由调用方按探测失败静默跳过(不阻断登记)。
func (s *service) probeServer(ctx context.Context, serverID string) (*PVEInfo, error) {
	// 1) pveversion(免提权):exit 0 = PVE 节点。
	pctx, cancel := runCtx(ctx)
	res, err := s.servers.Exec(pctx, serverID, []string{"pveversion"})
	cancel()
	if err != nil || res == nil || res.ExitCode != 0 {
		return nil, nil // 非 PVE(或探测不可达):按普通服务器处理。
	}

	// 2) 短主机名(识别 nodelist 里的自身,免提权)。
	pctx, cancel = runCtx(ctx)
	hostRes, err := s.servers.Exec(pctx, serverID, []string{"hostname", "-s"})
	cancel()
	selfName := ""
	if err == nil && hostRes != nil && hostRes.ExitCode == 0 {
		selfName = strings.TrimSpace(hostRes.Stdout)
	}

	// 3) corosync.conf(提权):集群配置。
	pctx, cancel = runCtx(ctx)
	confRes, confErr := s.runner.Run(pctx, serverID, []string{"cat", "/etc/pve/corosync.conf"})
	cancel()
	switch {
	case confErr == nil && confRes != nil:
		if info := parseCorosyncConf(confRes.Stdout); info != nil {
			info.SelfName = selfName
			return info, nil
		}
		// 配置在但解析不出(格式异常)→ 落到 pvecm 兜底。
	case confErr == nil: // cat 成功但退出码非 0 由 Runner 包为错误,理论到不了这里。
	case isNoPrivilege(confErr):
		// 无提权:退化为单机登记(集群信息待补 Token 后可见)。
		return standaloneInfo(selfName), nil
	case isNoSuchFile(confErr):
		// 提权可用但无 corosync.conf:真单机 PVE。
		return standaloneInfo(selfName), nil
	}

	// 4) pvecm nodes 兜底(pmxcfs 异常等;Name 列实为 ring0 地址)。
	pctx, cancel = runCtx(ctx)
	nodesRes, nodesErr := s.runner.Run(pctx, serverID, []string{"pvecm", "nodes"})
	cancel()
	if nodesErr == nil && nodesRes != nil {
		if info := parsePvecmNodes(nodesRes.Stdout); info != nil {
			info.SelfName = selfName
			return info, nil
		}
	}
	return standaloneInfo(selfName), nil
}

// standaloneInfo 组装单机 PVE 视图(节点=自身;地址由调用方按种子机登记地址补)。
func standaloneInfo(selfName string) *PVEInfo {
	if selfName == "" {
		selfName = "pve"
	}
	return &PVEInfo{ClusterName: selfName, Standalone: true, SelfName: selfName, Nodes: []NodeInfo{{Name: selfName}}}
}

func isNoPrivilege(err error) bool {
	return err != nil && strings.Contains(err.Error(), ErrNoPrivilege.Error())
}

// noSuchFileRe 匹配 cat 报错里的「文件不存在」(提权可用时即单机 PVE 的判据)。
var noSuchFileRe = regexp.MustCompile(`(?i)no such file`)

func isNoSuchFile(err error) bool {
	return err != nil && noSuchFileRe.MatchString(err.Error())
}

// parseCorosyncConf 解析 corosync.conf(totem.cluster_name + nodelist 的 name/ring0_addr)。
// 解析不出集群名或节点表 → nil(调用方走兜底)。花括号按深度追踪:node 块的 } 结束一个
// 节点,nodelist 自身的 } 结束节点表(两者同形,靠深度区分)。
func parseCorosyncConf(conf string) *PVEInfo {
	clusterName := ""
	var nodes []NodeInfo
	depth := 0
	inNodelist := false
	inNode := false
	cur := NodeInfo{}
	flush := func() {
		if inNode && cur.Addr != "" {
			if cur.Name == "" {
				cur.Name = cur.Addr
			}
			nodes = append(nodes, cur)
		}
		cur = NodeInfo{}
	}
	for _, raw := range strings.Split(conf, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		switch {
		case strings.HasPrefix(line, "nodelist") && strings.HasSuffix(line, "{"):
			inNodelist = true
			depth = 1
		case strings.HasSuffix(line, "{"):
			depth++
			if inNodelist && strings.HasPrefix(line, "node") {
				flush()
				inNode = true
			}
		case line == "}":
			if inNode {
				flush()
				inNode = false
			}
			depth--
			if inNodelist && depth <= 0 {
				inNodelist = false
			}
		default:
			key, val, ok := splitConfLine(line)
			if !ok {
				continue
			}
			if inNode {
				switch key {
				case "name":
					cur.Name = val
				case "ring0_addr", "ring_0_addr":
					cur.Addr = val
				}
				continue
			}
			if key == "cluster_name" {
				clusterName = val
			}
		}
	}
	if clusterName == "" || len(nodes) == 0 {
		return nil
	}
	return &PVEInfo{ClusterName: clusterName, Nodes: nodes}
}

// splitConfLine 拆 `key: value` 配置行。
func splitConfLine(line string) (string, string, bool) {
	i := strings.Index(line, ":")
	if i < 0 {
		return "", "", false
	}
	key := strings.TrimSpace(line[:i])
	val := strings.TrimSpace(line[i+1:])
	if key == "" || val == "" {
		return "", "", false
	}
	return key, val, true
}

// parsePvecmNodes 解析 `pvecm nodes` 输出(Cluster Name: 行 + 0x 节点行;Name 列为地址)。
// 解析不出 → nil。
func parsePvecmNodes(out string) *PVEInfo {
	clusterName := ""
	var nodes []NodeInfo
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "Name:") {
			parts := strings.SplitN(line, ":", 2)
			if v := strings.TrimSpace(parts[1]); v != "" {
				clusterName = v
			}
			continue
		}
		if strings.HasPrefix(line, "0x") {
			fields := strings.Fields(line)
			// 形如:0x00000001  1  A 10.0.0.1 (local);末尾 (local) 可选,地址取末位。
			if len(fields) < 2 {
				continue
			}
			cand := fields[len(fields)-1]
			if cand == "(local)" && len(fields) >= 3 {
				cand = fields[len(fields)-2]
			}
			if isAddrLike(cand) && !isNumericOnly(cand) {
				nodes = append(nodes, NodeInfo{Name: cand, Addr: cand})
			}
		}
	}
	if clusterName == "" || len(nodes) == 0 {
		return nil
	}
	return &PVEInfo{ClusterName: clusterName, Nodes: nodes}
}

// addrRe 是 ring0 地址的宽容白名单(域名/IPv4/IPv6)。
var addrRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.:\-_]{0,253}$`)

func isAddrLike(s string) bool { return addrRe.MatchString(s) }

// numericOnlyRe 匹配纯数字(排除 pvecm votes 列被误当地址)。
var numericOnlyRe = regexp.MustCompile(`^[0-9]+$`)

func isNumericOnly(s string) bool { return numericOnlyRe.MatchString(s) }
