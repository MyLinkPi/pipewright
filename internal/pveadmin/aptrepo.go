// aptrepo.go — PVE 节点 apt 仓库管理(SSH 读-改-备份-原子写)。
//
// 读:固定 sh 脚本遍历 /etc/apt/sources.list 与 sources.list.d/*.list|*.sources,
// ##PWFILE/##PWEND 标记分隔后 Go 解析(双格式:传统 one-line 与 deb822);条目按
// URI/组件分类 debian|pve|ceph|other。写:toggle(启停)/mirror(镜像预设)/
// preset(企业↔无订阅)三种动作,全部「重读最新 → Go 变换 → 回 parse 校验 →
// cp -a 备份 → WriteFile 写 tmp → mv -f 原子替换 → apt-get update 验证」
// (fstabCommit 同形);写入中途失败尽力回滚已提交文件。restore 从备份恢复。
//
// 纪律:路径白名单(拒穿越)、单文件 64KiB 上限、固定 argv 绝不拼 shell、
// Ceph 源只读(大版本升级须人工处理)、企业源不被镜像预设改写(镜像站不承载)。
// 镜像预设复用 cloudinit.go mirrorHosts;USTC/清华的 proxmox 镜像带 /proxmox
// 路径前缀(与 Debian 源纯主机替换不同);阿里云无 proxmox 镜像,PVE 源保持官方。
package pveadmin

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 仓库文件白名单:仅 sources.list 与 sources.list.d 下的 .list/.sources(拒穿越)。
var aptRepoPathRe = regexp.MustCompile(`^/etc/apt/sources\.list$|^/etc/apt/sources\.list\.d/[A-Za-z0-9][A-Za-z0-9._-]*\.(list|sources)$`)

// 备份文件名白名单:必须形如 <path>.bak.pipewright.<UTC 时间戳>。
var aptRepoBackupRe = regexp.MustCompile(`^/etc/apt/sources\.list(\.d/[A-Za-z0-9][A-Za-z0-9._-]*\.(list|sources))?\.bak\.pipewright\.[0-9]{8}T[0-9]{6}Z$`)

const (
	aptRepoCmdTO            = 30 * time.Second
	aptRepoUpdateTO         = 5 * time.Minute
	aptRepoFileMax          = 64 << 10  // 单文件上限
	aptRepoDumpMax          = 256 << 10 // 全部文件输出上限
	aptRepoApplyConcurrency = 4
)

// aptRepoDumpScript 是固定读取脚本(无插值;glob 无命中时 [ -f ] 为假自动跳过)。
const aptRepoDumpScript = `for f in /etc/apt/sources.list /etc/apt/sources.list.d/*.list /etc/apt/sources.list.d/*.sources; do
  if [ -f "$f" ]; then
    printf '##PWFILE:%s\n' "$f"
    cat "$f"
    printf '\n##PWEND\n'
  fi
done`

// ---- 对外类型 ---------------------------------------------------------------------------

// AptRepoEntry 是一个仓库条目(.list 的一行 / .sources 的一个 stanza)。
type AptRepoEntry struct {
	Key        string   `json:"key"` // 文件内稳定序号(list:行号;sources:stanza 序号,均 1-based)
	Enabled    bool     `json:"enabled"`
	Kind       string   `json:"kind"` // debian | pve | ceph | other
	Types      string   `json:"types,omitempty"`
	URIs       []string `json:"uris"`
	Suites     []string `json:"suites"`
	Components []string `json:"components"`
	SignedBy   string   `json:"signedBy,omitempty"`
	Raw        string   `json:"raw,omitempty"`
}

// AptRepoFile 是一个仓库文件。
type AptRepoFile struct {
	Path    string         `json:"path"`
	Format  string         `json:"format"` // list | sources
	Entries []AptRepoEntry `json:"entries"`
}

// AptRepoWarning 是仓库清单警告(level: warn | info)。
type AptRepoWarning struct {
	Key     string `json:"key"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

// AptRepoInventory 是节点仓库清单(只读视图)。
type AptRepoInventory struct {
	Node         string           `json:"node"`
	Codename     string           `json:"codename,omitempty"`
	Subscription string           `json:"subscription,omitempty"`
	Files        []AptRepoFile    `json:"files"`
	Warnings     []AptRepoWarning `json:"warnings"`
}

// AptRepoNodeSummary 是集群仓库总览里一个节点的摘要(派生自清单,不含逐条目)。
type AptRepoNodeSummary struct {
	Node         string           `json:"node"`
	Online       bool             `json:"online"`
	Codename     string           `json:"codename,omitempty"`
	Subscription string           `json:"subscription,omitempty"`
	PveRepo      string           `json:"pveRepo"` // enterprise | no-subscription | mixed | none
	Mirror       string           `json:"mirror"`  // official | ustc | tsinghua | aliyun | mixed
	Ceph         bool             `json:"ceph"`
	EnabledCount int              `json:"enabledCount"`
	Warnings     []AptRepoWarning `json:"warnings"`
	Error        string           `json:"error,omitempty"`
}

// AptRepoOverview 是集群仓库总览(逐节点错误隔离)。
type AptRepoOverview struct {
	Items []AptRepoNodeSummary `json:"items"`
}

// AptRepoToggleInput 是启停单条目入参。
type AptRepoToggleInput struct {
	Path    string `json:"path"`
	Key     string `json:"key"`
	Enabled bool   `json:"enabled"`
}

// AptRepoBackup 是一次写入产生的备份(供撤销)。
type AptRepoBackup struct {
	Path       string `json:"path"`
	BackupPath string `json:"backupPath"`
}

// AptRepoWriteResult 是仓库写入结果(文件写入成功 ≠ 索引刷新成功;401 等保留备份供撤销)。
type AptRepoWriteResult struct {
	Node       string          `json:"node"`
	OK         bool            `json:"ok"`
	Backups    []AptRepoBackup `json:"backups,omitempty"`
	Created    []string        `json:"created,omitempty"`
	UpdateOK   bool            `json:"updateOk"`
	UpdateTail string          `json:"updateTail,omitempty"`
	Note       string          `json:"note,omitempty"`
}

// AptRepoApplyInput 是批量应用入参(action: mirror | preset;nodes 空 = 全部在线节点)。
type AptRepoApplyInput struct {
	Action string   `json:"action"`
	Preset string   `json:"preset"`
	Nodes  []string `json:"nodes"`
}

// AptRepoApplyItem 是批量应用里一个节点的结果。
type AptRepoApplyItem struct {
	Node     string `json:"node"`
	OK       bool   `json:"ok"`
	Err      string `json:"err,omitempty"`
	UpdateOK bool   `json:"updateOk"`
	Note     string `json:"note,omitempty"`
}

// ---- 镜像预设 ---------------------------------------------------------------------------

// aptMirrorPreset 描述一个镜像预设的替换规则。debianHost/pveHost 空 = 恢复官方;
// pveSkip = PVE 源不动(阿里云无 proxmox 镜像);pvePrefix 是镜像站 proxmox 路径前缀。
type aptMirrorPreset struct {
	debianHost string
	pveHost    string
	pvePrefix  string
	pveSkip    bool
	note       string
}

var aptMirrorPresets = map[string]aptMirrorPreset{
	"official": {},
	"ustc":     {debianHost: mirrorHosts["ustc"], pveHost: mirrorHosts["ustc"], pvePrefix: "/proxmox"},
	"tsinghua": {debianHost: mirrorHosts["tsinghua"], pveHost: mirrorHosts["tsinghua"], pvePrefix: "/proxmox"},
	"aliyun":   {debianHost: mirrorHosts["aliyun"], pveSkip: true, note: "阿里云无 Proxmox 镜像,PVE 源保持官方"},
}

var (
	debianRepoHosts = map[string]bool{
		"deb.debian.org": true, "security.debian.org": true, "ftp.debian.org": true,
		"archive.ubuntu.com": true, "security.ubuntu.com": true, "ports.ubuntu.com": true,
	}
	pveRepoHosts    = map[string]bool{"download.proxmox.com": true, "enterprise.proxmox.com": true}
	mirrorRepoHosts = map[string]bool{mirrorHosts["ustc"]: true, mirrorHosts["tsinghua"]: true, mirrorHosts["aliyun"]: true}
)

// rewriteAptURI 按预设改写一个仓库 URI(返回新值与是否变化)。ceph/other 不改;
// 企业源不被镜像预设改写(镜像站不承载订阅源)。
//
// Debian 源做路径规范化(参考 pvetools 行为语义,净室重写):security 源路径恒为
// /debian-security、主源恒为 /debian、ubuntu 恒为 /ubuntu(ports 为 /ubuntu-ports),
// 按 suite 语义判定而非保留原始路径——原始行路径缺失/非标准(如裸主机
// http://security.debian.org)时,换源后仍产出可用的规范镜像 URL,绝不把损坏路径
// 带到镜像站。
func rewriteAptURI(raw, kind string, enterprise bool, p aptMirrorPreset, suites []string) (string, bool) {
	if kind != "debian" && kind != "pve" {
		return raw, false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw, false
	}
	host := u.Hostname()
	if kind == "debian" {
		isUbuntu := strings.Contains(host, "ubuntu") || strings.Contains(u.Path, "ubuntu")
		// ports.ubuntu.com 的路径是 /ubuntu-ports(非 /ubuntu),须单独保留,否则
		// 会把 arm Ubuntu 源改坏(旧逻辑只换主机不动路径,这里是回归防护)。
		isUbuntuPorts := host == "ports.ubuntu.com" || u.Path == "/ubuntu-ports" || strings.HasPrefix(u.Path, "/ubuntu-ports/")
		canonPath := "/debian"
		switch {
		case isUbuntuPorts:
			canonPath = "/ubuntu-ports"
		case isUbuntu:
			canonPath = "/ubuntu"
		case isSecuritySuite(suites):
			canonPath = "/debian-security"
		}
		if p.debianHost == "" { // 恢复官方:按 suite/路径推导官方主机 + 规范路径
			if !mirrorRepoHosts[host] && !debianRepoHosts[host] {
				return raw, false
			}
			target := "deb.debian.org"
			switch {
			case isUbuntuPorts:
				target = "ports.ubuntu.com"
			case isUbuntu && isSecuritySuite(suites):
				target = "security.ubuntu.com"
			case isUbuntu:
				target = "archive.ubuntu.com"
			case canonPath == "/debian-security":
				target = "security.debian.org"
			}
			u.Host = target
			u.Path = canonPath
			nu := u.String()
			return nu, nu != raw
		}
		if debianRepoHosts[host] || mirrorRepoHosts[host] { // 换镜像:规范路径
			u.Scheme = "https"
			u.Host = p.debianHost
			u.Path = canonPath
			nu := u.String()
			return nu, nu != raw
		}
		return raw, false
	}
	// pve
	if enterprise || p.pveSkip {
		return raw, false
	}
	if p.pveHost == "" { // 恢复官方:剥 /proxmox 前缀
		if mirrorRepoHosts[host] && strings.HasPrefix(u.Path, "/proxmox") {
			u.Host = "download.proxmox.com"
			u.Path = strings.TrimPrefix(u.Path, "/proxmox")
			nu := u.String()
			return nu, nu != raw
		}
		return raw, false
	}
	if pveRepoHosts[host] { // 官方 → 镜像:插前缀
		u.Scheme = "https"
		u.Host = p.pveHost
		u.Path = p.pvePrefix + u.Path
		nu := u.String()
		return nu, nu != raw
	}
	if mirrorRepoHosts[host] && strings.HasPrefix(u.Path, "/proxmox") && host != p.pveHost { // 镜像 → 镜像
		u.Host = p.pveHost
		nu := u.String()
		return nu, nu != raw
	}
	return raw, false
}

// isSecuritySuite 判定是否 security 源 suite:bookworm-security 等新式、stable-security
// 滚动别名、buster/updates 旧式(Debian ≤10)均归为 security(路径 /debian-security)。
func isSecuritySuite(suites []string) bool {
	for _, s := range suites {
		if strings.HasSuffix(s, "-security") || s == "stable-security" || strings.HasSuffix(s, "/updates") {
			return true
		}
	}
	return false
}

// classifyAptRepo 按 URI/组件分类条目(镜像站同时承载 debian 与 proxmox,须看路径)。
func classifyAptRepo(uris, components []string) (kind string, enterprise bool) {
	for _, c := range components {
		if c == "pve-enterprise" {
			return "pve", true
		}
	}
	kind = "other"
	for _, uri := range uris {
		host, path := "", uri
		if u, err := url.Parse(uri); err == nil {
			host, path = u.Hostname(), u.Path
		}
		switch {
		case strings.Contains(path, "/ceph") && (strings.Contains(host, "proxmox.com") || mirrorRepoHosts[host]):
			return "ceph", strings.Contains(host, "enterprise.proxmox.com")
		case strings.Contains(host, "proxmox.com") || strings.Contains(path, "/debian/pve"):
			kind = "pve"
			if strings.Contains(host, "enterprise.proxmox.com") {
				enterprise = true
			}
		case debianRepoHosts[host] || mirrorRepoHosts[host] || strings.Contains(path, "/debian") || strings.Contains(path, "/ubuntu"):
			if kind == "other" {
				kind = "debian"
			}
		}
	}
	for _, c := range components {
		if c == "pve-no-subscription" || c == "pvetest" {
			kind = "pve"
		}
	}
	return kind, enterprise
}

// ---- 内部表示与解析 ---------------------------------------------------------------------

// aptEntry 是解析出的条目(引用原始行下标,变换直接改 lines)。
type aptEntry struct {
	key        string
	lineIdx    int
	endIdx     int // .list: lineIdx+1;.sources: stanza 结束(不含)
	enabled    bool
	kind       string
	enterprise bool
	types      []string
	uris       []string
	suites     []string
	components []string
	signedBy   string
	opts       string // .list 方括号选项原文
	raw        string
}

// aptRepoFileRaw 是一个文件的原始行 + 条目。
type aptRepoFileRaw struct {
	path    string
	format  string
	lines   []string
	entries []aptEntry
}

func (f *aptRepoFileRaw) render() string {
	return strings.Join(f.lines, "\n") + "\n"
}

func (f *aptRepoFileRaw) findEntry(key string) *aptEntry {
	for i := range f.entries {
		if f.entries[i].key == key {
			return &f.entries[i]
		}
	}
	return nil
}

// splitDebLine 拆 one-line 仓库行:类型 + [选项] + uri suite components...。
func splitDebLine(line string) (typ, opts string, rest []string, ok bool) {
	s := strings.TrimSpace(line)
	i := strings.IndexAny(s, " \t")
	if i < 0 {
		return "", "", nil, false
	}
	typ = s[:i]
	if typ != "deb" && typ != "deb-src" {
		return "", "", nil, false
	}
	s = strings.TrimSpace(s[i:])
	if strings.HasPrefix(s, "[") {
		j := strings.Index(s, "]")
		if j < 0 {
			return "", "", nil, false
		}
		opts = s[:j+1]
		s = strings.TrimSpace(s[j+1:])
	}
	rest = strings.Fields(s)
	if len(rest) < 2 {
		return "", "", nil, false
	}
	return typ, opts, rest, true
}

func signedByFromOpts(opts string) string {
	for _, f := range strings.Fields(strings.Trim(opts, "[]")) {
		if strings.HasPrefix(f, "signed-by=") {
			return strings.TrimPrefix(f, "signed-by=")
		}
	}
	return ""
}

func parseListFile(path string, lines []string) *aptRepoFileRaw {
	f := &aptRepoFileRaw{path: path, format: "list", lines: lines}
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		enabled, body := true, t
		if strings.HasPrefix(t, "#") {
			body = strings.TrimSpace(strings.TrimLeft(t, "#"))
			enabled = false
		}
		typ, opts, rest, ok := splitDebLine(body)
		if !ok {
			continue
		}
		e := aptEntry{
			key: strconv.Itoa(i + 1), lineIdx: i, endIdx: i + 1, enabled: enabled,
			types: []string{typ}, uris: []string{rest[0]}, suites: []string{rest[1]}, components: rest[2:],
			signedBy: signedByFromOpts(opts), opts: opts, raw: ln,
		}
		e.kind, e.enterprise = classifyAptRepo(e.uris, e.components)
		f.entries = append(f.entries, e)
	}
	return f
}

func parseSourcesFile(path string, lines []string) *aptRepoFileRaw {
	f := &aptRepoFileRaw{path: path, format: "sources", lines: lines}
	start := -1
	var stanza []string
	flush := func(end int) {
		if start < 0 {
			return
		}
		e := aptEntry{lineIdx: start, endIdx: end, enabled: true, raw: strings.Join(stanza, "\n")}
		for _, ln := range stanza {
			t := strings.TrimSpace(ln)
			if t == "" || strings.HasPrefix(t, "#") {
				continue
			}
			k, v, ok := splitConfLine(t)
			if !ok {
				continue
			}
			switch strings.ToLower(k) {
			case "types":
				e.types = strings.Fields(v)
			case "uris":
				e.uris = strings.Fields(v)
			case "suites":
				e.suites = strings.Fields(v)
			case "components":
				e.components = strings.Fields(v)
			case "signed-by":
				e.signedBy = v
			case "enabled":
				e.enabled = !(strings.EqualFold(v, "no") || strings.EqualFold(v, "false") || v == "0")
			}
		}
		if len(e.uris) > 0 && len(e.suites) > 0 {
			e.kind, e.enterprise = classifyAptRepo(e.uris, e.components)
			f.entries = append(f.entries, e)
		}
		start, stanza = -1, nil
	}
	for i, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			flush(i)
			continue
		}
		if start < 0 {
			start = i
		}
		stanza = append(stanza, ln)
	}
	flush(len(lines))
	for i := range f.entries {
		f.entries[i].key = strconv.Itoa(i + 1)
	}
	return f
}

// parseAptDump 解析 ##PWFILE/##PWEND 标记分隔的多文件输出。
func parseAptDump(out string) ([]*aptRepoFileRaw, error) {
	if len(out) > aptRepoDumpMax {
		return nil, fmt.Errorf("%w: 仓库文件总输出超过 256KiB 上限", ErrInvalidInput)
	}
	var files []*aptRepoFileRaw
	var cur *aptRepoFileRaw
	var buf []string
	flush := func() {
		if cur == nil {
			return
		}
		content := strings.Join(buf, "\n")
		content = strings.TrimSuffix(content, "\n") // 去掉脚本在 ##PWEND 前补的一个换行
		if len(content) > aptRepoFileMax {
			return // 超限文件跳过(调用方按缺失处理;正常 apt 源远小于此)
		}
		lines := strings.Split(content, "\n")
		if cur.format == "sources" {
			files = append(files, parseSourcesFile(cur.path, lines))
		} else {
			files = append(files, parseListFile(cur.path, lines))
		}
		cur, buf = nil, nil
	}
	for _, ln := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(ln, "##PWFILE:"):
			flush()
			path := strings.TrimPrefix(ln, "##PWFILE:")
			if !aptRepoPathRe.MatchString(path) {
				continue // 白名单外路径丢弃(防御)
			}
			cur = &aptRepoFileRaw{path: path, format: "list"}
			if strings.HasSuffix(path, ".sources") {
				cur.format = "sources"
			}
		case ln == "##PWEND":
			flush()
		default:
			if cur != nil {
				buf = append(buf, ln)
			}
		}
	}
	flush()
	return files, nil
}

// readAptRepoFiles SSH 读取节点全部仓库文件 + Debian 代号。
func (s *service) readAptRepoFiles(ctx context.Context, cl *Cluster, node string) ([]*aptRepoFileRaw, string, error) {
	sid, err := s.nodeServerID(ctx, cl, node)
	if err != nil {
		return nil, "", err
	}
	rctx, cancel := context.WithTimeout(ctx, aptRepoCmdTO)
	defer cancel()
	res, err := s.runner.Run(rctx, sid, []string{"sh", "-c", aptRepoDumpScript})
	if err != nil {
		return nil, "", err
	}
	files, perr := parseAptDump(res.Stdout)
	if perr != nil {
		return nil, "", perr
	}
	codename := ""
	cres, cerr := s.runner.Run(rctx, sid, []string{"cat", "/etc/os-release"})
	if cerr == nil && cres != nil {
		codename = osReleaseCodename(cres.Stdout)
	}
	return files, codename, nil
}

// osReleaseCodename 解析 /etc/os-release 的 VERSION_CODENAME(KEY=VALUE 格式,
// 与 corosync 的 KEY: VALUE 不同;容忍引号)。
func osReleaseCodename(out string) string {
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimSpace(ln)
		i := strings.IndexByte(ln, '=')
		if i <= 0 {
			continue
		}
		if strings.TrimSpace(ln[:i]) == "VERSION_CODENAME" {
			return strings.Trim(strings.TrimSpace(ln[i+1:]), `"`)
		}
	}
	return ""
}

// ---- 变换 -------------------------------------------------------------------------------

// toggleEntry 启停一个条目(.list 注释/去注释;.sources Enabled 字段)。
func (f *aptRepoFileRaw) toggleEntry(e *aptEntry, enabled bool) {
	if f.format == "list" {
		if enabled {
			f.lines[e.lineIdx] = strings.TrimLeft(f.lines[e.lineIdx], "# \t")
		} else {
			f.lines[e.lineIdx] = "# " + f.lines[e.lineIdx]
		}
		e.enabled = enabled
		return
	}
	val := "no"
	if enabled {
		val = "yes"
	}
	f.setSourcesField(e, "Enabled", val)
	e.enabled = enabled
}

// setSourcesField 设置 stanza 字段(存在则原位替换,缺失则 stanza 末尾插入)。
// 插入会移动后续行下标:多条目变换须按 lineIdx 降序调用。
func (f *aptRepoFileRaw) setSourcesField(e *aptEntry, field, value string) {
	for i := e.lineIdx; i < e.endIdx && i < len(f.lines); i++ {
		t := strings.TrimSpace(f.lines[i])
		if k, _, ok := splitConfLine(t); ok && strings.EqualFold(k, field) {
			indent := f.lines[i][:len(f.lines[i])-len(strings.TrimLeft(f.lines[i], " \t"))]
			f.lines[i] = indent + field + ": " + value
			return
		}
	}
	nl := make([]string, 0, len(f.lines)+1)
	nl = append(nl, f.lines[:e.endIdx]...)
	nl = append(nl, field+": "+value)
	nl = append(nl, f.lines[e.endIdx:]...)
	f.lines = nl
}

// applyMirror 对全部 debian/pve 条目应用镜像预设(返回是否有变化)。
func (f *aptRepoFileRaw) applyMirror(p aptMirrorPreset) bool {
	changed := false
	for i := range f.entries {
		e := &f.entries[i]
		var newURIs []string
		any := false
		for _, uri := range e.uris {
			nu, ok := rewriteAptURI(uri, e.kind, e.enterprise, p, e.suites)
			newURIs = append(newURIs, nu)
			any = any || ok
		}
		if !any {
			continue
		}
		changed = true
		if f.format == "list" {
			for k, old := range e.uris {
				if newURIs[k] != old {
					f.lines[e.lineIdx] = strings.Replace(f.lines[e.lineIdx], old, newURIs[k], 1)
				}
			}
		} else {
			f.setSourcesField(e, "URIs", strings.Join(newURIs, " "))
		}
		e.uris = newURIs
	}
	return changed
}

// applySuite 把 debian/pve 条目的 suite 词元 from→to(大版本升级 bookworm→trixie;
// 只动 suite 字段,不碰 URI/组件;bookworm-security → trixie-security 一并覆盖)。
func (f *aptRepoFileRaw) applySuite(from, to string) bool {
	changed := false
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(from) + `\b`)
	for i := range f.entries {
		e := &f.entries[i]
		if e.kind != "debian" && e.kind != "pve" {
			continue
		}
		hit := false
		for _, st := range e.suites {
			if re.MatchString(st) {
				hit = true
			}
		}
		if !hit {
			continue
		}
		changed = true
		var ns []string
		for _, st := range e.suites {
			ns = append(ns, re.ReplaceAllString(st, to))
		}
		if f.format == "list" {
			ln := f.lines[e.lineIdx]
			if idx := strings.Index(ln, e.uris[0]); idx >= 0 {
				after := ln[idx+len(e.uris[0]):]
				f.lines[e.lineIdx] = ln[:idx+len(e.uris[0])] + re.ReplaceAllString(after, to)
			}
		} else {
			f.setSourcesField(e, "Suites", strings.Join(ns, " "))
		}
		e.suites = ns
	}
	return changed
}

// applySubscriptionPreset 企业↔无订阅一键切换(返回是否有变化;缺失侧由调用方建文件)。
// 按 lineIdx 降序处理(.sources 插入 Enabled 行会移动后续下标)。
func (f *aptRepoFileRaw) applySubscriptionPreset(wantEnterprise bool) bool {
	changed := false
	for i := len(f.entries) - 1; i >= 0; i-- {
		e := &f.entries[i]
		if e.kind != "pve" {
			continue
		}
		want := e.enterprise == wantEnterprise
		if e.enabled != want {
			f.toggleEntry(e, want)
			changed = true
		}
	}
	return changed
}

// hasPveSide 判断文件群是否已有指定侧的 pve 条目(企业/公共)。
func hasPveSide(files []*aptRepoFileRaw, enterprise bool) bool {
	for _, f := range files {
		for _, e := range f.entries {
			if e.kind == "pve" && e.enterprise == enterprise {
				return true
			}
		}
	}
	return false
}

// ---- 提交(备份 → 原子写 → apt update 验证) ----------------------------------------------

// aptFileChange 是一个待提交文件。
type aptFileChange struct {
	path    string
	content string
	isNew   bool
}

// commitAptFiles 逐文件备份+原子替换,随后 apt-get update 验证;中途失败尽力回滚。
// updateTO 是索引刷新时限(仓库管理 5min;大版本换源后 15min)。
func (s *service) commitAptFiles(ctx context.Context, cl *Cluster, node string, changes []aptFileChange, updateTO time.Duration) (*AptRepoWriteResult, error) {
	sid, err := s.nodeServerID(ctx, cl, node)
	if err != nil {
		return nil, err
	}
	out := &AptRepoWriteResult{Node: node}
	ts := time.Now().UTC().Format("20060102T150405Z")
	var done []AptRepoBackup
	rollback := func() {
		for _, b := range done {
			rctx, cancel := context.WithTimeout(context.Background(), aptRepoCmdTO)
			_, _ = s.runner.Run(rctx, sid, []string{"cp", "-a", b.BackupPath, b.Path})
			cancel()
		}
	}
	for _, ch := range changes {
		if !aptRepoPathRe.MatchString(ch.path) {
			rollback()
			return nil, fmt.Errorf("%w: 仓库路径 %q 不在白名单内", ErrInvalidInput, ch.path)
		}
		if len(ch.content) > aptRepoFileMax {
			rollback()
			return nil, fmt.Errorf("%w: 仓库文件 %s 超过 64KiB 上限", ErrInvalidInput, ch.path)
		}
		rctx, cancel := context.WithTimeout(ctx, aptRepoCmdTO)
		if !ch.isNew {
			bak := ch.path + ".bak.pipewright." + ts
			if _, berr := s.runner.Run(rctx, sid, []string{"cp", "-a", ch.path, bak}); berr != nil {
				cancel()
				rollback()
				return nil, fmt.Errorf("%w: 备份 %s 失败: %v", ErrProbe, ch.path, unwrapTail(berr))
			}
			done = append(done, AptRepoBackup{Path: ch.path, BackupPath: bak})
		}
		tmp := ch.path + ".pw-tmp"
		werr := s.runner.WriteFile(rctx, sid, tmp, 0o644, strings.NewReader(ch.content))
		if werr == nil {
			_, werr = s.runner.Run(rctx, sid, []string{"mv", "-f", tmp, ch.path})
		}
		cancel()
		if werr != nil {
			rollback()
			return nil, fmt.Errorf("%w: 写入 %s 失败: %v", ErrProbe, ch.path, unwrapTail(werr))
		}
		if ch.isNew {
			out.Created = append(out.Created, ch.path)
		}
	}
	out.Backups = done
	out.OK = true
	// apt-get update 验证(失败不否定写入:保留备份供撤销,401 等由 UI 提示)。
	uctx, ucancel := context.WithTimeout(ctx, updateTO)
	defer ucancel()
	res, uerr := s.runner.Run(uctx, sid, []string{"apt-get", "update"})
	out.UpdateOK = uerr == nil
	if res != nil {
		out.UpdateTail = tailOutput(res)
	}
	if uerr != nil && res == nil {
		out.UpdateTail = unwrapTail(uerr)
	}
	return out, nil
}

// aptUpdateVerify 单独跑 apt-get update 验证(restore 用)。
func (s *service) aptUpdateVerify(ctx context.Context, sid string, out *AptRepoWriteResult) {
	uctx, cancel := context.WithTimeout(ctx, aptRepoUpdateTO)
	defer cancel()
	res, err := s.runner.Run(uctx, sid, []string{"apt-get", "update"})
	out.UpdateOK = err == nil
	if res != nil {
		out.UpdateTail = tailOutput(res)
	}
	if err != nil && res == nil {
		out.UpdateTail = unwrapTail(err)
	}
}

// ---- 领域方法 ---------------------------------------------------------------------------

// ListAptRepos 读节点仓库清单 + 警告(只读;订阅状态经 API 尽力而为,Token 未配也可用)。
func (s *service) ListAptRepos(ctx context.Context, id, node string) (*AptRepoInventory, error) {
	if !nameRe.MatchString(node) {
		return nil, fmt.Errorf("%w: 节点名不合规", ErrInvalidInput)
	}
	cl, err := s.clusterByID(ctx, id)
	if err != nil {
		return nil, err
	}
	files, codename, err := s.readAptRepoFiles(ctx, cl, node)
	if err != nil {
		return nil, err
	}
	inv := &AptRepoInventory{Node: node, Codename: codename, Files: make([]AptRepoFile, 0, len(files))}
	if _, c, cerr := s.clientFor(ctx, id); cerr == nil {
		var sb apiSubscription
		if gerr := c.Get(ctx, "/api2/json/nodes/"+node+"/subscription", &sb); gerr == nil {
			inv.Subscription = sb.Status
		} else {
			inv.Subscription = "notfound"
		}
	}
	for _, f := range files {
		pf := AptRepoFile{Path: f.path, Format: f.format, Entries: make([]AptRepoEntry, 0, len(f.entries))}
		for _, e := range f.entries {
			pf.Entries = append(pf.Entries, AptRepoEntry{
				Key: e.key, Enabled: e.enabled, Kind: e.kind, Types: strings.Join(e.types, " "),
				URIs: e.uris, Suites: e.suites, Components: e.components, SignedBy: e.signedBy, Raw: e.raw,
			})
		}
		inv.Files = append(inv.Files, pf)
	}
	inv.Warnings = aptRepoWarnings(files, codename, inv.Subscription)
	return inv, nil
}

// isStableSuiteAlias 判定滚动稳定 suite(stable / stable-security / stable-updates / …):
// 它随 Debian 稳定版漂移,PVE 节点必须钉到具体运行代号(实测教训见 aptRepoWarnings)。
func isStableSuiteAlias(st string) bool {
	return st == "stable" || strings.HasPrefix(st, "stable-")
}

// aptRepoWarnings 计算清单警告(企业源无订阅/suite 不符/stable 别名漂移/Ceph 只读/无 PVE 源)。
// 返回值恒为非 nil(零警告也返回空数组):序列化为 [] 而非 null,前端可安全遍历。
func aptRepoWarnings(files []*aptRepoFileRaw, codename, subscription string) []AptRepoWarning {
	ws := []AptRepoWarning{}
	enterpriseEnabled, cephSeen, pveEnabled := false, false, false
	suiteMismatch := false
	stableAlias := false
	for _, f := range files {
		for _, e := range f.entries {
			switch e.kind {
			case "ceph":
				cephSeen = true
			case "pve":
				if e.enabled {
					pveEnabled = true
					if e.enterprise {
						enterpriseEnabled = true
					}
				}
			}
			if e.enabled && codename != "" {
				for _, st := range e.suites {
					if isStableSuiteAlias(st) {
						stableAlias = true
						continue // 专项警告覆盖,不重复计入泛化不符
					}
					base := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(st, "-security"), "-updates"), "-backports")
					if base != codename && (e.kind == "debian" || e.kind == "pve") {
						suiteMismatch = true
					}
				}
			}
		}
	}
	if enterpriseEnabled && !strings.EqualFold(subscription, "active") {
		ws = append(ws, AptRepoWarning{Key: "enterprise-no-sub", Level: "warn",
			Message: "企业源(pve-enterprise)已启用但无有效订阅,apt update 将 401;请切换到 no-subscription 或激活订阅"})
	}
	if suiteMismatch {
		ws = append(ws, AptRepoWarning{Key: "suite-mismatch", Level: "warn",
			Message: "存在 suite 与运行代号(" + codename + ")不符的启用条目,请核对是否残留旧版本源"})
	}
	if stableAlias {
		ws = append(ws, AptRepoWarning{Key: "stable-alias", Level: "warn",
			Message: "存在 stable 系滚动 suite:它随 Debian 稳定版漂移(2026-11 起 stable-security 已指向 trixie),与 bookworm 的 PVE 8 混用会让 dist-upgrade 试图卸载 proxmox-ve(被 pve-apt-hook 拦截);应用镜像可自动钉到运行代号"})
	}
	if !pveEnabled {
		ws = append(ws, AptRepoWarning{Key: "no-pve-repo", Level: "warn", Message: "未启用任何 PVE 软件源,将无法收到 PVE 更新"})
	}
	if cephSeen {
		ws = append(ws, AptRepoWarning{Key: "ceph-readonly", Level: "info",
			Message: "检测到 Ceph 源(只读展示):Ceph 集群的大版本升级须人工按官方流程处理"})
	}
	return ws
}

// mirrorHostPreset 是镜像站域名 → 预设名(mirrorHosts 反向)。
var mirrorHostPreset = func() map[string]string {
	m := make(map[string]string, len(mirrorHosts))
	for k, h := range mirrorHosts {
		m[h] = k
	}
	return m
}()

// aptRepoSummary 从清单派生总览摘要:PVE 源类型/镜像/Ceph/启用条目数。
// PveRepo 看启用的 pve 条目企业/公共两侧;Mirror 看启用 debian+pve 条目主机里出现的
// 非官方镜像预设(0=official,1=该名,>1=mixed;aliyun 预设下 PVE 保持官方仍计 aliyun);
// Ceph 任意 ceph 条目存在即真(与 aptRepoWarnings 口径一致)。
func aptRepoSummary(files []*aptRepoFileRaw) (pveRepo, mirror string, ceph bool, enabledCount int) {
	entEnabled, pubEnabled := false, false
	mirrors := map[string]bool{}
	for _, f := range files {
		for _, e := range f.entries {
			if e.kind == "ceph" {
				ceph = true
			}
			if !e.enabled {
				continue
			}
			enabledCount++
			if e.kind == "pve" {
				if e.enterprise {
					entEnabled = true
				} else {
					pubEnabled = true
				}
			}
			if e.kind == "debian" || e.kind == "pve" {
				for _, uri := range e.uris {
					if u, err := url.Parse(uri); err == nil {
						if preset, ok := mirrorHostPreset[u.Hostname()]; ok {
							mirrors[preset] = true
						}
					}
				}
			}
		}
	}
	switch {
	case entEnabled && pubEnabled:
		pveRepo = "mixed"
	case entEnabled:
		pveRepo = "enterprise"
	case pubEnabled:
		pveRepo = "no-subscription"
	default:
		pveRepo = "none"
	}
	switch len(mirrors) {
	case 0:
		mirror = "official"
	case 1:
		for k := range mirrors {
			mirror = k
		}
	default:
		mirror = "mixed"
	}
	return pveRepo, mirror, ceph, enabledCount
}

// ListAptRepoOverview 集群仓库总览(只读;服务端扇出,逐节点错误隔离,照 ListUpdates)。
func (s *service) ListAptRepoOverview(ctx context.Context, id string) (*AptRepoOverview, error) {
	cl, c, err := s.clientFor(ctx, id)
	if err != nil {
		return nil, err
	}
	cn, err := fetchClusterNodes(ctx, c)
	if err != nil {
		return nil, err
	}
	out := &AptRepoOverview{Items: make([]AptRepoNodeSummary, len(cn.Nodes))}
	fanOutNodes(len(cn.Nodes), aptRepoApplyConcurrency, func(i int) {
		n := cn.Nodes[i]
		// Warnings 恒为非 nil:出错/离线节点的摘要也要给前端合法形状(空数组),
		// 否则 JSON null 会让前端遍历崩溃(整个 tab 白屏)。
		item := AptRepoNodeSummary{Node: n.Name, Online: n.Online, Warnings: []AptRepoWarning{}}
		defer func() { out.Items[i] = item }()
		if !n.Online {
			item.Error = "节点离线"
			return
		}
		nctx, cancel := context.WithTimeout(ctx, updatesNodeTO)
		defer cancel()
		files, codename, rerr := s.readAptRepoFiles(nctx, cl, n.Name)
		if rerr != nil {
			item.Error = unwrapTail(rerr)
			return
		}
		item.Codename = codename
		var sb apiSubscription
		if gerr := c.Get(nctx, "/api2/json/nodes/"+n.Name+"/subscription", &sb); gerr == nil {
			item.Subscription = sb.Status
		} else {
			item.Subscription = "notfound"
		}
		item.PveRepo, item.Mirror, item.Ceph, item.EnabledCount = aptRepoSummary(files)
		item.Warnings = aptRepoWarnings(files, codename, item.Subscription)
	})
	return out, nil
}

// ToggleAptRepo 启停单条目(ceph 拒绝;重读最新防 TOCTOU,按 key 定位)。
func (s *service) ToggleAptRepo(ctx context.Context, id, node string, in AptRepoToggleInput) (*AptRepoWriteResult, error) {
	if !nameRe.MatchString(node) {
		return nil, fmt.Errorf("%w: 节点名不合规", ErrInvalidInput)
	}
	if !aptRepoPathRe.MatchString(in.Path) {
		return nil, fmt.Errorf("%w: 仓库路径不合规", ErrInvalidInput)
	}
	cl, err := s.clusterByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if cl.Readonly {
		return nil, fmt.Errorf("%w: 集群 %s 处于只读模式,请在设置页解锁后再修改仓库", ErrReadonly, cl.Name)
	}
	files, _, err := s.readAptRepoFiles(ctx, cl, node)
	if err != nil {
		return nil, err
	}
	var target *aptRepoFileRaw
	for _, f := range files {
		if f.path == in.Path {
			target = f
			break
		}
	}
	if target == nil {
		return nil, fmt.Errorf("%w: 节点 %s 上不存在仓库文件 %s", ErrInvalidInput, node, in.Path)
	}
	e := target.findEntry(in.Key)
	if e == nil {
		return nil, fmt.Errorf("%w: 条目 %s 不存在(文件可能已变化,请刷新)", ErrInvalidInput, in.Key)
	}
	if e.kind == "ceph" {
		return nil, fmt.Errorf("%w: Ceph 源只读,须人工管理", ErrInvalidInput)
	}
	if e.enabled == in.Enabled {
		return &AptRepoWriteResult{Node: node, OK: true, UpdateOK: true, Note: "条目已是目标状态,无需变更"}, nil
	}
	target.toggleEntry(e, in.Enabled)
	return s.commitAptFiles(ctx, cl, node, []aptFileChange{{path: target.path, content: target.render()}}, aptRepoUpdateTO)
}

// SetAptRepoMirror 应用镜像预设(debian+pve;ceph/other/企业源不动)。
// 非官方预设额外补齐缺失条目,保证切源后得到完整可用配置(pvetools 语义):
//   - PVE 非订阅源(PVE 更新的唯一来源,缺失则本功能无从升级):禁用则启用,
//     完全没有则按预设基址新建标准源文件;阿里云无 proxmox 镜像 → 官方 download.proxmox.com;
//   - Debian backports:完全缺失才补建(组件沿用受管理主源,追加同文件),
//     已有(含禁用)不动,尊重管理员显式配置。
func (s *service) SetAptRepoMirror(ctx context.Context, id, node, preset string) (*AptRepoWriteResult, error) {
	p, ok := aptMirrorPresets[preset]
	if !ok {
		return nil, fmt.Errorf("%w: 未知镜像预设 %q(official/ustc/tsinghua/aliyun)", ErrInvalidInput, preset)
	}
	cl, err := s.clusterByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if cl.Readonly {
		return nil, fmt.Errorf("%w: 集群 %s 处于只读模式,请在设置页解锁后再修改仓库", ErrReadonly, cl.Name)
	}
	files, codename, err := s.readAptRepoFiles(ctx, cl, node)
	if err != nil {
		return nil, err
	}
	dirty := map[string]string{}
	created := map[string]bool{}
	for _, f := range files {
		if f.applyMirror(p) {
			dirty[f.path] = f.render()
		}
	}
	// 稳定别名钉到运行代号(官方/镜像预设都做):pvetools 风格模板写的 stable-security
	// 会随 Debian 稳定版漂移——2026-11 起 stable-security=trixie-security,混进 bookworm
	// 的 PVE 8 节点后依赖解析会卸载 proxmox-ve(pve-apt-hook 拦截,dist-upgrade 失败,
	// 实测)。钉死代号后 rolling 别名不再漂移;非稳定 suite(testing 等)不动。
	if codename != "" {
		for _, f := range files {
			if f.applySuite("stable", codename) {
				dirty[f.path] = f.render()
			}
		}
	}
	var notes []string
	if p.debianHost != "" || p.pveHost != "" { // 非官方预设:补齐缺失条目(官方恢复保持纯还原语义)
		// ensure 会原地变异 files(启用/追加),每项只调用一次,变更以最终 render 为准。
		for _, en := range []func([]*aptRepoFileRaw, string, aptMirrorPreset) ([]aptFileChange, string){
			ensurePveNoSubscription, ensureDebianBackports,
		} {
			chs, note := en(files, codename, p)
			for _, ch := range chs {
				dirty[ch.path] = ch.content
				if ch.isNew {
					created[ch.path] = true
				}
			}
			if note != "" {
				notes = append(notes, note)
			}
		}
	}
	if len(dirty) == 0 {
		return &AptRepoWriteResult{Node: node, OK: true, UpdateOK: true, Note: "无需变更" + noteSuffix(strings.Join(notes, ";"))}, nil
	}
	var changes []aptFileChange
	for path, content := range dirty {
		changes = append(changes, aptFileChange{path: path, content: content, isNew: created[path]})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].path < changes[j].path })
	res, err := s.commitAptFiles(ctx, cl, node, changes, aptRepoUpdateTO)
	if err != nil {
		return nil, err
	}
	res.Note = strings.TrimSpace(strings.Join(notes, ";") + noteSuffix(p.note))
	return res, nil
}

// pveNoSubBase 返回预设下 PVE 非订阅源基址(镜像站;阿里云无 proxmox 镜像 → 官方)。
func (p aptMirrorPreset) pveNoSubBase() string {
	if p.pveHost != "" {
		return "https://" + p.pveHost + "/proxmox/debian/pve"
	}
	return "http://download.proxmox.com/debian/pve"
}

// debianMirrorBase 返回预设下 Debian 源基址(官方预设返回空 = 不补建)。
func (p aptMirrorPreset) debianMirrorBase() string {
	if p.debianHost == "" {
		return ""
	}
	return "https://" + p.debianHost + "/debian"
}

// ensurePveNoSubscription 保证存在「已启用」的 PVE 非订阅源(PVE 更新的唯一来源):
// 有禁用条目 → 启用;完全没有 → 按预设基址新建标准源文件。缺代号 → 跳过并说明。
// 返回文件变更与人话说明(已就绪返回空)。
func ensurePveNoSubscription(files []*aptRepoFileRaw, codename string, p aptMirrorPreset) ([]aptFileChange, string) {
	hasEnabled := false
	var disabledFile *aptRepoFileRaw
	var disabledEntry *aptEntry
	for _, f := range files {
		for i := range f.entries {
			e := &f.entries[i]
			if e.kind != "pve" || e.enterprise {
				continue
			}
			if e.enabled {
				hasEnabled = true
			} else if disabledFile == nil {
				disabledFile, disabledEntry = f, &f.entries[i]
			}
		}
	}
	if hasEnabled {
		return nil, ""
	}
	if disabledFile != nil {
		disabledFile.toggleEntry(disabledEntry, true)
		return []aptFileChange{{path: disabledFile.path, content: disabledFile.render()}}, "已启用原禁用的 PVE 非订阅源"
	}
	if codename == "" {
		return nil, "无法识别 Debian 代号,未补建 PVE 非订阅源"
	}
	content := "# Managed by Pipewright (镜像切换补建)\ndeb " + p.pveNoSubBase() + " " + codename + " pve-no-subscription\n"
	return []aptFileChange{{path: "/etc/apt/sources.list.d/pve-no-subscription.list", content: content, isNew: true}}, "已补建 PVE 非订阅源"
}

// ensureDebianBackports 补建缺失的 Debian backports 源(镜像预设语义 = 完整可用配置):
// 仅当完全没有 backports 条目时,在与受管理主源(宿主受管理 + suite 为纯代号 + 已启用)
// 同文件追加一行(组件沿用主源);已有(含禁用)不动,尊重管理员显式配置。
func ensureDebianBackports(files []*aptRepoFileRaw, codename string, p aptMirrorPreset) ([]aptFileChange, string) {
	base := p.debianMirrorBase()
	if base == "" || codename == "" {
		return nil, ""
	}
	for _, f := range files {
		for i := range f.entries {
			e := &f.entries[i]
			if e.kind != "debian" {
				continue
			}
			for _, st := range e.suites {
				if strings.HasSuffix(st, "-backports") {
					return nil, "" // 已有(启用或禁用):尊重现状
				}
			}
		}
	}
	var refFile *aptRepoFileRaw
	var refEntry *aptEntry
	for _, f := range files {
		for i := range f.entries {
			e := &f.entries[i]
			if e.kind != "debian" || !e.enabled || len(e.uris) == 0 {
				continue
			}
			u, uerr := url.Parse(e.uris[0])
			if uerr != nil || u.Host == "" || !(debianRepoHosts[u.Hostname()] || mirrorRepoHosts[u.Hostname()]) {
				continue
			}
			for _, st := range e.suites {
				if st == codename {
					refFile, refEntry = f, &f.entries[i]
					break
				}
			}
			if refFile != nil {
				break
			}
		}
		if refFile != nil {
			break
		}
	}
	if refFile == nil || refEntry == nil {
		return nil, "未找到受管理的主源条目,未补建 backports 源"
	}
	refFile.lines = append(refFile.lines, "deb "+base+" "+codename+"-backports "+strings.Join(refEntry.components, " "))
	return []aptFileChange{{path: refFile.path, content: refFile.render()}}, "已补建 backports 源"
}

func noteSuffix(note string) string {
	if note == "" {
		return ""
	}
	return "(" + note + ")"
}

// SetAptRepoPreset 企业↔无订阅一键切换(缺失侧按标准内容 + 运行代号建文件)。
func (s *service) SetAptRepoPreset(ctx context.Context, id, node, preset string) (*AptRepoWriteResult, error) {
	var wantEnterprise bool
	switch preset {
	case "no-subscription":
	case "enterprise":
		wantEnterprise = true
	default:
		return nil, fmt.Errorf("%w: 未知订阅预设 %q(no-subscription/enterprise)", ErrInvalidInput, preset)
	}
	if !nameRe.MatchString(node) {
		return nil, fmt.Errorf("%w: 节点名不合规", ErrInvalidInput)
	}
	cl, err := s.clusterByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if cl.Readonly {
		return nil, fmt.Errorf("%w: 集群 %s 处于只读模式,请在设置页解锁后再修改仓库", ErrReadonly, cl.Name)
	}
	files, codename, err := s.readAptRepoFiles(ctx, cl, node)
	if err != nil {
		return nil, err
	}
	var changes []aptFileChange
	for _, f := range files {
		if f.applySubscriptionPreset(wantEnterprise) {
			changes = append(changes, aptFileChange{path: f.path, content: f.render()})
		}
	}
	if !hasPveSide(files, wantEnterprise) {
		if codename == "" {
			return nil, fmt.Errorf("%w: 无法识别 Debian 代号(/etc/os-release 缺 VERSION_CODENAME),不能创建标准源", ErrInvalidInput)
		}
		path, line := "/etc/apt/sources.list.d/pve-no-subscription.list",
			fmt.Sprintf("deb http://download.proxmox.com/debian/pve %s pve-no-subscription", codename)
		if wantEnterprise {
			path = "/etc/apt/sources.list.d/pve-enterprise.list"
			line = fmt.Sprintf("deb https://enterprise.proxmox.com/debian/pve %s pve-enterprise", codename)
		}
		content := "# Managed by Pipewright (订阅预设切换)\n" + line + "\n"
		changes = append(changes, aptFileChange{path: path, content: content, isNew: true})
	}
	if len(changes) == 0 {
		return &AptRepoWriteResult{Node: node, OK: true, UpdateOK: true, Note: "无需变更"}, nil
	}
	return s.commitAptFiles(ctx, cl, node, changes, aptRepoUpdateTO)
}

// RestoreAptRepo 从备份恢复仓库文件(backupPath 白名单校验)并刷新索引验证。
func (s *service) RestoreAptRepo(ctx context.Context, id, node, path, backupPath string) (*AptRepoWriteResult, error) {
	if !nameRe.MatchString(node) {
		return nil, fmt.Errorf("%w: 节点名不合规", ErrInvalidInput)
	}
	if !aptRepoPathRe.MatchString(path) || !aptRepoBackupRe.MatchString(backupPath) {
		return nil, fmt.Errorf("%w: 路径/备份名不合规", ErrInvalidInput)
	}
	if !strings.HasPrefix(backupPath, path+".bak.pipewright.") {
		return nil, fmt.Errorf("%w: 备份与目标文件不对应", ErrInvalidInput)
	}
	cl, err := s.clusterByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if cl.Readonly {
		return nil, fmt.Errorf("%w: 集群 %s 处于只读模式,请在设置页解锁后再恢复仓库", ErrReadonly, cl.Name)
	}
	sid, err := s.nodeServerID(ctx, cl, node)
	if err != nil {
		return nil, err
	}
	rctx, cancel := context.WithTimeout(ctx, aptRepoCmdTO)
	defer cancel()
	if _, rerr := s.runner.Run(rctx, sid, []string{"cp", "-a", backupPath, path}); rerr != nil {
		return nil, fmt.Errorf("%w: 恢复备份失败: %v", ErrProbe, unwrapTail(rerr))
	}
	out := &AptRepoWriteResult{Node: node, OK: true, Backups: []AptRepoBackup{{Path: path, BackupPath: backupPath}}}
	s.aptUpdateVerify(ctx, sid, out)
	return out, nil
}

// ApplyAptRepos 批量应用镜像/订阅预设(并发闸,逐节点结果隔离)。
func (s *service) ApplyAptRepos(ctx context.Context, id string, in AptRepoApplyInput) ([]AptRepoApplyItem, error) {
	if in.Action != "mirror" && in.Action != "preset" {
		return nil, fmt.Errorf("%w: 批量动作仅支持 mirror/preset", ErrInvalidInput)
	}
	cl, c, err := s.clientFor(ctx, id)
	if err != nil {
		return nil, err
	}
	if cl.Readonly {
		return nil, fmt.Errorf("%w: 集群 %s 处于只读模式,请在设置页解锁后再修改仓库", ErrReadonly, cl.Name)
	}
	cn, err := fetchClusterNodes(ctx, c)
	if err != nil {
		return nil, err
	}
	nodes, err := resolveUpgradeNodes(in.Nodes, cn, true)
	if err != nil {
		return nil, err
	}
	items := make([]AptRepoApplyItem, len(nodes))
	fanOutNodes(len(nodes), aptRepoApplyConcurrency, func(i int) {
		node := nodes[i]
		item := AptRepoApplyItem{Node: node}
		defer func() { items[i] = item }()
		var res *AptRepoWriteResult
		var aerr error
		if in.Action == "mirror" {
			res, aerr = s.SetAptRepoMirror(ctx, id, node, in.Preset)
		} else {
			res, aerr = s.SetAptRepoPreset(ctx, id, node, in.Preset)
		}
		if aerr != nil {
			item.Err = unwrapTail(aerr)
			return
		}
		item.OK, item.UpdateOK, item.Note = res.OK, res.UpdateOK, res.Note
	})
	return items, nil
}
