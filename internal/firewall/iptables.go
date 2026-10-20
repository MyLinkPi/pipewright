// iptables.go 是规则的执行层:Host 抽象、提权判定、命令构造(纯函数,供 golden 单测)
// 与 应用/清理/断言 三个动作。命令一律 array 形式,绝不拼 shell(AC-SEC-02)。
//
// 挂钩规则刻意不含端口/网段(只有 comment 标签 + 跳转),端口匹配全部放在专用链内部:
// 挂钩 spec 与当时的端口配置无关,清理(-D 需逐字匹配)与幂等断言(-C)才可靠 —— 改过
// 端口配置后,旧挂钩依然删得掉、查得到。
package firewall

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
)

// 链与标签常量(全部规则/挂钩以此识别归属,清理只动自己的东西)。
const (
	chainInput  = "PW-MESH-GUARD"   // INPUT 侧(主机进程端口)
	chainDocker = "PW-MESH-GUARD-D" // DOCKER-USER 侧(容器发布端口)
	ruleComment = "pipewright:mesh-guard"
)

// Host 抽象「平台自身所在主机」的命令执行(生产基于 os/exec;测试注入 fake)。
// 命令以 array 传入,绝不经 shell 解释。
type Host interface {
	// Run 同步执行 name+args;err 非 nil 表示无法启动(命令不存在等,exitCode<0);
	// 跑完非零退出不算 err(退出码在 exitCode)。
	Run(ctx context.Context, name string, args []string) (stdout, stderr string, exitCode int, err error)
}

// localHost 是基于 os/exec 的默认 Host 实现(ctx 取消即 kill)。
type localHost struct{}

func (localHost) Run(ctx context.Context, name string, args []string) (string, string, int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return outBuf.String(), errBuf.String(), ee.ExitCode(), nil
		}
		return outBuf.String(), errBuf.String(), -1, err
	}
	return outBuf.String(), errBuf.String(), 0, nil
}

// osStat 是 os.Stat 的可注入别名(inContainerMarker 用;单测可改写)。
var osStat = os.Stat

// logLine 是包内统一日志前缀(KeepAlive 后台路径无 HTTP 响应可承载错误)。
func logLine(format string, args ...any) {
	log.Printf("[firewall] "+format, args...)
}

// privilege 描述本机的一次提权方式(一次动作内复用同一判定;与 platformhttps 同款):
//
//	root → prefix nil,直接执行;免密 sudo → prefix ["sudo","-n"]。
type privilege struct {
	prefix []string
}

// iptables 二进制的探测候选(PATH 之外补 sbin:非 root 登录 shell 的 PATH 常缺)。
var iptablesCandidates = []string{"iptables", "/usr/sbin/iptables", "/sbin/iptables", "/usr/local/sbin/iptables"}
var ip6tablesCandidates = []string{"ip6tables", "/usr/sbin/ip6tables", "/sbin/ip6tables", "/usr/local/sbin/ip6tables"}

// resolveBinary 依次探测候选(--version 不需要特权),返回第一个可用路径;全不可用返回 ""。
func resolveBinary(ctx context.Context, h Host, candidates []string) string {
	for _, c := range candidates {
		if _, _, code, err := h.Run(ctx, c, []string{"--version"}); err == nil && code == 0 {
			return c
		}
	}
	return ""
}

// buildPrivilege 判定提权方式:root(id -u == 0)直行;否则 sudo -n true 探免密。
// 两者皆无由调用方按 ErrNoPrivilege 报人话(此处只给前缀)。
func buildPrivilege(ctx context.Context, h Host) privilege {
	if out, _, code, err := h.Run(ctx, "id", []string{"-u"}); err == nil && code == 0 && strings.TrimSpace(out) == "0" {
		return privilege{}
	}
	if _, _, code, err := h.Run(ctx, "sudo", []string{"-n", "true"}); err == nil && code == 0 {
		return privilege{prefix: []string{"sudo", "-n"}}
	}
	return privilege{}
}

// ---------- 命令构造(纯函数,单测 golden) ----------

// commentArgs 返回规则标签段(所有自建规则统一携带,便于人工排查识别归属)。
func commentArgs() []string { return []string{"-m", "comment", "--comment", ruleComment} }

// inputChainRules 生成 INPUT 侧专用链的规则序列(先 -N 容忍已存在,再 -F 重建内容):
// 命中端口的流量 ESTABLISHED → 放行网段 ACCEPT → DROP;未命中端口 RETURN 回 INPUT;
// lo 全放行(本机可信)。ESTABLISHED 首位保证应用动作本身不断管理员已建立的会话
// (被锁外时旧会话仍可进 UI 执行回退/禁用,只是无法通过确认门)。
func inputChainRules(bin string, ports []int, cidrs []string) [][]string {
	cmds := [][]string{
		{bin, "-N", chainInput},
		{bin, "-F", chainInput},
	}
	hit := func(extra ...string) []string {
		return append([]string{bin, "-A", chainInput, "-p", "tcp", "-m", "multiport", "--dports", joinPorts(ports)}, extra...)
	}
	cmds = append(cmds, hit("-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"))
	cmds = append(cmds, []string{bin, "-A", chainInput, "-i", "lo", "-j", "ACCEPT"})
	for _, c := range cidrs {
		cmds = append(cmds, hit("-s", c, "-j", "ACCEPT"))
	}
	cmds = append(cmds, hit("-j", "DROP"))
	cmds = append(cmds, []string{bin, "-A", chainInput, "-j", "RETURN"})
	return cmds
}

// dockerChainRules 生成 DOCKER-USER 侧专用链规则:命中端口的转发流量 ESTABLISHED/
// 放行网段 RETURN(继续走 docker 自己的链),其余 DROP;未命中端口一律 RETURN。
// 容器发布端口的流量走 FORWARD 不走 INPUT,必须在此链限制;本机进程 push registry
// 的流量不经 FORWARD,不受影响。
func dockerChainRules(bin string, ports []int, cidrs []string) [][]string {
	cmds := [][]string{
		{bin, "-N", chainDocker},
		{bin, "-F", chainDocker},
	}
	hit := func(extra ...string) []string {
		return append([]string{bin, "-A", chainDocker, "-p", "tcp", "-m", "multiport", "--dports", joinPorts(ports)}, extra...)
	}
	cmds = append(cmds, hit("-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "RETURN"))
	for _, c := range cidrs {
		cmds = append(cmds, hit("-s", c, "-j", "RETURN"))
	}
	cmds = append(cmds, hit("-j", "DROP"))
	cmds = append(cmds, []string{bin, "-A", chainDocker, "-j", "RETURN"})
	return cmds
}

// inputHookRule / dockerHookRule 生成挂钩规则体(-C 校验 / -I 插入 / -D 删除共用同一
// 构造,spec 与端口配置无关,幂等判定才可靠)。返回完整命令形态(首元素为二进制名)。
func inputHookRule(bin string) []string {
	return append([]string{bin}, append(commentArgs(), "-j", chainInput)...)
}

func dockerHookRule(bin string) []string {
	return append([]string{bin}, append(commentArgs(), "-j", chainDocker)...)
}

// cleanupCommands 生成单族(v4 或 v6)的清理序列:删挂钩 → 清链 → 删链(v4 另含
// docker 侧)。删挂钩的 spec 与端口无关,改过配置的旧挂钩同样删得掉。
func cleanupCommands(bin string, docker bool) [][]string {
	hook := inputHookRule(bin)
	cmds := [][]string{
		append([]string{bin, "-D", "INPUT"}, hook[1:]...),
		{bin, "-F", chainInput},
		{bin, "-X", chainInput},
	}
	if docker {
		dhook := dockerHookRule(bin)
		cmds = append(cmds,
			append([]string{bin, "-D", "DOCKER-USER"}, dhook[1:]...),
			[]string{bin, "-F", chainDocker},
			[]string{bin, "-X", chainDocker},
		)
	}
	return cmds
}

// splitCIDRs 按地址族拆分放行网段(mesh 虚拟网是 IPv4;补充网段允许 IPv6)。
func splitCIDRs(cidrs []string) (v4, v6 []string) {
	for _, c := range cidrs {
		if ip, _, err := net.ParseCIDR(c); err == nil {
			if ip.To4() != nil {
				v4 = append(v4, c)
			} else {
				v6 = append(v6, c)
			}
		}
	}
	return v4, v6
}

// ---------- 执行动作 ----------

// runPriv 跑一条提权命令,返回 (stdout, stderr, exitCode, 启动错误)。
func runPriv(ctx context.Context, h Host, p privilege, cmd []string) (string, string, int, error) {
	full := append(append([]string{}, p.prefix...), cmd...)
	return h.Run(ctx, full[0], full[1:])
}

// execStrict 要求退出码 0(建链/加规则/挂钩失败 = 硬错误)。
func execStrict(ctx context.Context, h Host, p privilege, cmd []string) error {
	_, stderr, code, err := runPriv(ctx, h, p, cmd)
	if err != nil {
		return fmt.Errorf("%s: %w", cmd[0], err)
	}
	if code != 0 {
		return fmt.Errorf("%v 失败(退出码 %d):%s", cmd, code, truncate(strings.TrimSpace(stderr), 300))
	}
	return nil
}

// ensureChainCmd 跑一条建链序列命令:-N 容忍已存在(真正失败会被随后的 -F 暴露),
// 其余严格要求成功。
func ensureChainCmd(ctx context.Context, h Host, p privilege, cmd []string) error {
	if len(cmd) > 1 && cmd[1] == "-N" {
		_, _, _, _ = runPriv(ctx, h, p, cmd)
		return nil
	}
	return execStrict(ctx, h, p, cmd)
}

// ensureHook 幂等挂链:-C 校验规则体已在 chain 上则跳过;不在则 -I 插到首位。
// hook 是完整命令形态(首元素为二进制名),去掉首元素即规则体。
func ensureHook(ctx context.Context, h Host, p privilege, bin, chain string, hook []string) error {
	spec := hook[1:]
	if _, _, code, err := runPriv(ctx, h, p, append([]string{bin, "-C", chain}, spec...)); err == nil && code == 0 {
		return nil // 已在位
	}
	return execStrict(ctx, h, p, append([]string{bin, "-I", chain, "1"}, spec...))
}

// dockerUserChainExists 探测 DOCKER-USER 链是否在(无 Docker/未启动则不在)。
func dockerUserChainExists(ctx context.Context, h Host, p privilege, bin string) bool {
	_, _, code, err := runPriv(ctx, h, p, []string{bin, "-n", "-L", chainDocker})
	return err == nil && code == 0
}

// applyRules 全量收敛一套规则(v4 INPUT + v4 DOCKER-USER + v6 INPUT),返回人话 notes
// (如「DOCKER-USER 不存在,容器发布端口未限制」「ip6tables 缺失,IPv6 未限制」)。
// 任一硬步骤失败即返回错误(调用方负责清理回滚)。
func (s *service) applyRules(ctx context.Context, ports []int, cidrs []string) ([]string, error) {
	d := s.detect(ctx)
	if d.Iptables == "" {
		return nil, ErrNoIptables
	}
	if !d.IsRoot && !d.SudoOk {
		return nil, ErrNoPrivilege
	}
	p := buildPrivilege(ctx, s.host)
	v4, v6 := splitCIDRs(cidrs)
	var notes []string

	// v4:INPUT 链 + 挂钩(主机进程端口)。
	for _, cmd := range inputChainRules(d.Iptables, ports, v4) {
		if err := ensureChainCmd(ctx, s.host, p, cmd); err != nil {
			return nil, err
		}
	}
	if err := ensureHook(ctx, s.host, p, d.Iptables, "INPUT", inputHookRule(d.Iptables)); err != nil {
		return nil, err
	}

	// v4:DOCKER-USER 侧(容器发布端口);链不存在 = 无 Docker,记 note 跳过。
	if dockerUserChainExists(ctx, s.host, p, d.Iptables) {
		for _, cmd := range dockerChainRules(d.Iptables, ports, v4) {
			if err := ensureChainCmd(ctx, s.host, p, cmd); err != nil {
				return nil, err
			}
		}
		if err := ensureHook(ctx, s.host, p, d.Iptables, "DOCKER-USER", dockerHookRule(d.Iptables)); err != nil {
			return nil, err
		}
	} else {
		notes = append(notes, "DOCKER-USER 链不存在(无 Docker 或未启动),容器发布端口未限制")
	}

	// v6:同构 INPUT 限制(防 IPv6 绕过;best-effort)。
	if d.Ip6tables != "" {
		for _, cmd := range inputChainRules(d.Ip6tables, ports, v6) {
			if err := ensureChainCmd(ctx, s.host, p, cmd); err != nil {
				return nil, err
			}
		}
		if err := ensureHook(ctx, s.host, p, d.Ip6tables, "INPUT", inputHookRule(d.Ip6tables)); err != nil {
			return nil, err
		}
	} else {
		notes = append(notes, "未找到 ip6tables,IPv6 入口未限制")
	}
	return notes, nil
}

// cleanupRules 拆除我们自己的全部规则(v4 含 docker 侧 + v6)。容忍「本就不在」
// (删除目标已不存在 = 已达目标态);真正的执行失败(特权丢失等)汇总为首错返回。
func (s *service) cleanupRules(ctx context.Context) error {
	d := s.detect(ctx)
	if d.Iptables == "" && d.Ip6tables == "" {
		return nil // 无工具即无我们可清理之物(如容器形态拒绝后调用)
	}
	p := buildPrivilege(ctx, s.host)
	var firstErr error
	record := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if d.Iptables != "" {
		for _, cmd := range cleanupCommands(d.Iptables, true) {
			record(execStrict(ctx, s.host, p, cmd))
		}
	}
	if d.Ip6tables != "" {
		for _, cmd := range cleanupCommands(d.Ip6tables, false) {
			record(execStrict(ctx, s.host, p, cmd))
		}
	}
	return firstErr
}

// assertRules 断言固化版规则仍在位(KeepAlive 每分钟一拍):只查各挂钩(-C,与端口
// 无关);任一漂移(机器重启/人工 iptables -F)即整套重放。
func (s *service) assertRules(ctx context.Context, ports []int, cidrs []string) error {
	d := s.detect(ctx)
	if d.Iptables == "" {
		return ErrNoIptables
	}
	if !d.IsRoot && !d.SudoOk {
		return ErrNoPrivilege
	}
	p := buildPrivilege(ctx, s.host)
	drifted := false
	check := func(bin, chain string, hook []string) {
		if _, _, code, err := runPriv(ctx, s.host, p, append([]string{bin, "-C", chain}, hook[1:]...)); err != nil || code != 0 {
			drifted = true
		}
	}
	check(d.Iptables, "INPUT", inputHookRule(d.Iptables))
	if dockerUserChainExists(ctx, s.host, p, d.Iptables) {
		check(d.Iptables, "DOCKER-USER", dockerHookRule(d.Iptables))
	}
	if d.Ip6tables != "" {
		check(d.Ip6tables, "INPUT", inputHookRule(d.Ip6tables))
	}
	if !drifted {
		return nil
	}
	logLine("固化规则漂移,重放 %d 端口 / %d 网段", len(ports), len(cidrs))
	_, err := s.applyRules(ctx, ports, cidrs)
	return err
}

// trimErr 取人话摘要(错误文本前 300 字)。
func trimErr(err error) string {
	if err == nil {
		return ""
	}
	return truncate(err.Error(), 300)
}
