// install.go — ssar 安装包部署到目标机 + 安装状态探测。
//
// 安装(单台,幂等 = 重装/升级同路径):
//  1. 免提权探测 uname -s/-m 与包管理器(rpm/dpkg) —— 非 Linux / aarch64(暂无发行
//     资产)/ 两者皆无 → 明确人读错误;
//  2. 从平台缓存选包(Downloader.PackagePath;未缓存 → 提示先下载);
//  3. 上传到 /tmp(登录用户可写),提权执行 rpm -Uvh --replacepkgs / dpkg -i ——
//     包 %post 自动装 systemd 服务并 enable+start(无 systemd 的容器环境回落 cron);
//  4. 清理远端临时包(尽力而为),免提权复探 ssar -V 验证。
//
// 状态探测(免提权;##PW: 分段的静态脚本):ssar -V(版本)、sresar 存活
// (systemctl is-active,无 systemctl 回落 pgrep)、uname。结果按服务器缓存
// (statusTTL),页面轮询不打 SSH。所有命令均为纯静态文本(AC-SEC-02)。
package ssar

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// statusTTL 是状态探测缓存时长(机器监控页轮询间隔量级)。
const statusTTL = 30 * time.Second

// installTimeout 是单台安装的整体超时(上传 ~3MB 包 + 包管理器安装 + 复探)。
const installTimeout = 3 * time.Minute

// probeTimeout 是单次状态探测超时。
const probeTimeout = 15 * time.Second

// 段名(状态/环境探测脚本的 ##PW: 标记)。
const (
	secEnv    = "env"
	secFamily = "family"
	secSsar   = "ssarver"
	secSresar = "sresar"
	secEnd    = "end"
)

// ssarMarker 与 6-1 服务器指标脚本同款的段分隔标记。
const ssarMarker = "##PW:"

// probeArgs 返回状态/环境探测命令(sh -c 静态脚本):
//
//	echo ##PW:env; uname -s; uname -m
//	echo ##PW:family; command -v rpm; command -v dpkg
//	echo ##PW:ssarver; ssar -V
//	echo ##PW:sresar; <sresar 存活判定>
//	echo ##PW:end
//
// 各命令失败只让该段为空(解析层降级),不影响其它段。
func probeArgs() []string {
	parts := []string{
		`echo "` + ssarMarker + secEnv + `"`,
		`uname -s 2>/dev/null`,
		`uname -m 2>/dev/null`,
		`echo "` + ssarMarker + secFamily + `"`,
		`command -v rpm 2>/dev/null`,
		`command -v dpkg 2>/dev/null`,
		`echo "` + ssarMarker + secSsar + `"`,
		`ssar -V 2>/dev/null`,
		`echo "` + ssarMarker + secSresar + `"`,
		`if command -v systemctl >/dev/null 2>&1; then systemctl is-active sresar 2>/dev/null; else pgrep -x sresar >/dev/null 2>&1 && echo active || echo inactive; fi`,
		`echo "` + ssarMarker + secEnd + `"`,
	}
	return []string{"sh", "-c", strings.Join(parts, "; ")}
}

// splitSections 按 ##PW:<名> 标记行切分脚本 stdout(段名 → 段文本)。首个标记之前的
// 内容丢弃;标记后无输出即空段(命令缺失/失败的常态)。
func splitSections(out string) map[string]string {
	sections := map[string]string{}
	cur := ""
	var buf strings.Builder
	flush := func() {
		if cur != "" {
			sections[cur] = buf.String()
		}
	}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.HasPrefix(line, ssarMarker) {
			flush()
			cur = strings.TrimSpace(strings.TrimPrefix(line, ssarMarker))
			buf.Reset()
			continue
		}
		if cur != "" {
			buf.WriteString(line)
			buf.WriteByte('\n')
		}
	}
	flush()
	return sections
}

// probeSSH 跑一次探测脚本并解析为 Status(不做缓存;连接/执行错误原样返回)。
func (s *service) probeSSH(ctx context.Context, serverID string) (Status, error) {
	st := Status{ServerID: serverID}
	res, err := s.runner.RunPlain(ctx, serverID, probeArgs())
	if err != nil {
		// 非零退出码(如某命令缺/systemctl 异常)也带 stdout —— 仍按段解析尽力而为。
		if res == nil {
			return st, err
		}
	}
	sec := splitSections(res.Stdout)
	lines := func(name string) []string {
		var out []string
		for _, l := range strings.Split(strings.TrimSpace(sec[name]), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				out = append(out, l)
			}
		}
		return out
	}
	env := lines(secEnv)
	if len(env) >= 2 {
		st.OS, st.Arch = env[0], env[1]
	}
	if ver, ok := parseSsarVersion(sec[secSsar]); ok {
		st.Installed = true
		st.Version = ver
	}
	st.CollectorActive = strings.TrimSpace(sec[secSresar]) == "active"
	if !st.Installed && st.OS != "" && st.OS != "Linux" {
		st.Error = fmt.Sprintf("仅支持 Linux 目标机(探测到 %s)", st.OS)
		return st, nil
	}
	return st, nil
}

// parseSsarVersion 从 `ssar -V` 输出解析版本(形如 "ssar 1.0.5";取首个非空行的
// 最后一个空白分隔字段,容忍各版本前后缀差异)。
func parseSsarVersion(s string) (string, bool) {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		return fields[len(fields)-1], true
	}
	return "", false
}

// Status 探测单台(带 TTL 缓存)。服务器不存在 → ErrServerNotFound 原样上抛。
func (s *service) Status(ctx context.Context, serverID string) (Status, error) {
	s.statusMu.Lock()
	if e, ok := s.statusCache[serverID]; ok && time.Since(e.at) < statusTTL {
		s.statusMu.Unlock()
		return e.status, nil
	}
	s.statusMu.Unlock()

	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	st, err := s.probeSSH(pctx, serverID)
	if err != nil {
		return Status{ServerID: serverID, Error: humanProbeError(err)}, nil
	}
	s.statusMu.Lock()
	s.statusCache[serverID] = statusEntry{status: st, at: time.Now()}
	s.statusMu.Unlock()
	return st, nil
}

// humanProbeError 把探测失败映射为人读文案(不含凭据/内部栈)。
func humanProbeError(err error) string {
	msg := err.Error()
	if strings.Contains(msg, "认证失败") || strings.Contains(msg, "auth") {
		return "SSH 认证失败:密钥或口令无效"
	}
	if strings.Contains(msg, "不可达") || strings.Contains(msg, "unreachable") {
		return "无法连接服务器:主机不可达或超时"
	}
	return "状态探测失败:连接或命令执行错误"
}

// detectFamily 从探测脚本 family 段判定包家族(rpm 优先于 dpkg;皆无 → false)。
func detectFamily(sec map[string]string) (string, bool) {
	fam := strings.TrimSpace(sec[secFamily])
	switch {
	case strings.Contains(fam, "rpm"):
		return "rpm", true
	case strings.Contains(fam, "dpkg"):
		return "deb", true
	default:
		return "", false
	}
}

// remotePkgPath 是安装包在目标机的临时落位(固定名,重装自然覆盖)。version 经
// sanitizeVersion 清洗(GitHub tag 理论上可含 "/" 等,远端路径只留安全字符)。
func remotePkgPath(family, version string) string {
	return fmt.Sprintf("/tmp/pipewright-ssar-%s.%s", sanitizeVersion(version), family)
}

// sanitizeVersion 把 release tag 归一为远端文件名安全字符(其余替换为 "-")。
func sanitizeVersion(tag string) string {
	var b strings.Builder
	for _, r := range tag {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "pkg"
	}
	return b.String()
}

// installOne 对单台执行完整安装流程(探测 → 选包 → 上传 → 提权安装 → 清理 → 验证)。
// 永不返回 error:成败都落到 InstallResult(批量端点逐台独立,单台失败不连累其它台)。
func (s *service) installOne(ctx context.Context, serverID string) InstallResult {
	out := InstallResult{ServerID: serverID}

	ictx, cancel := context.WithTimeout(ctx, installTimeout)
	defer cancel()

	// ① 探测环境(直采,不吃缓存:安装决策必须基于当下机器状态)。
	pctx, pcancel := context.WithTimeout(ictx, probeTimeout)
	defer pcancel()
	res, err := s.runner.RunPlain(pctx, serverID, probeArgs())
	if err != nil && res == nil {
		out.Error = "环境探测失败:" + humanProbeError(err)
		return out
	}
	sec := splitSections(res.Stdout)
	envLines := strings.Fields(strings.TrimSpace(sec[secEnv]))
	if len(envLines) < 2 {
		out.Error = "环境探测失败:无法获取 uname 结果"
		return out
	}
	osName, arch := envLines[0], envLines[1]
	if osName != "Linux" {
		out.Error = fmt.Sprintf("仅支持 Linux 目标机(探测到 %s)", osName)
		return out
	}
	family, ok := detectFamily(sec)
	if !ok {
		out.Error = "目标机既无 rpm 也无 dpkg,无法安装"
		return out
	}

	// ② 选平台缓存包(aarch64 等暂无资产的架构在此报明确错误)。
	var verTag string
	if s.downloader != nil {
		verTag, _ = cachedPackages(s.downloader.binDir())
	}
	pkgPath, err := s.downloaderPackage(family, arch)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	version := strings.TrimSpace(verTag)
	if version == "" {
		version = "pkg"
	}

	// ③ 上传到 /tmp(登录用户可写,无需提权)。
	f, err := os.Open(pkgPath)
	if err != nil {
		out.Error = "读取平台缓存的安装包失败:" + err.Error()
		return out
	}
	remote := remotePkgPath(family, version)
	uploadErr := s.runner.Upload(ictx, serverID, f, remote)
	_ = f.Close()
	if uploadErr != nil {
		out.Error = "上传安装包失败:" + humanProbeError(uploadErr)
		return out
	}

	// ④ 提权安装(rpm -Uvh --replacepkgs 幂等重装;dpkg -i 本身幂等)。
	var installCmd []string
	if family == "rpm" {
		installCmd = []string{"rpm", "-Uvh", "--replacepkgs", remote}
	} else {
		installCmd = []string{"dpkg", "-i", remote}
	}
	if _, err := s.runner.Run(ictx, serverID, installCmd); err != nil {
		out.Error = "安装失败(需要 root 登录或免密 sudo):" + truncateErr(err)
		// 安装失败也清掉临时包(尽力而为)。
		_, _ = s.runner.Run(ictx, serverID, []string{"rm", "-f", remote})
		return out
	}

	// ⑤ 清理远端临时包 + 免提权复探验证。
	_, _ = s.runner.Run(ictx, serverID, []string{"rm", "-f", remote})
	vctx, vcancel := context.WithTimeout(ictx, probeTimeout)
	defer vcancel()
	st, perr := s.probeSSH(vctx, serverID)
	if perr == nil && st.Installed {
		s.statusMu.Lock()
		s.statusCache[serverID] = statusEntry{status: st, at: time.Now()}
		s.statusMu.Unlock()
		out.OK = true
		out.Version = st.Version
		return out
	}
	out.Error = "安装命令已执行,但 ssar 仍不可用(请检查包管理器输出 / PATH)"
	return out
}

// downloaderPackage 包一层(下载器未配置时给人读错误而非 panic)。
func (s *service) downloaderPackage(family, arch string) (string, error) {
	if s.downloader == nil {
		return "", fmt.Errorf("ssar 安装包下载器未配置")
	}
	return s.downloader.PackagePath(family, arch)
}

// truncateErr 裁剪错误文本(人读上限;不含凭据)。
func truncateErr(err error) string {
	msg := err.Error()
	if len(msg) > 400 {
		msg = msg[:400] + "…"
	}
	return msg
}

// Install 把缓存的安装包部署到 serverIDs 各机(有界并发,逐台独立成败)。
func (s *service) Install(ctx context.Context, serverIDs []string) []InstallResult {
	results := make([]InstallResult, len(serverIDs))
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, id := range serverIDs {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = s.installOne(ctx, id)
		}(i, id)
	}
	wg.Wait()
	return results
}

// BinStatus 透传下载器状态(未配置返回空状态)。
func (s *service) BinStatus() BinStatus {
	if s.downloader == nil {
		return BinStatus{}
	}
	return s.downloader.BinStatus()
}

// TriggerDownload 异步触发安装包下载(下载器未配置为 no-op)。
func (s *service) TriggerDownload(ctx context.Context) {
	if s.downloader != nil {
		s.downloader.TriggerDownload(ctx)
	}
}
