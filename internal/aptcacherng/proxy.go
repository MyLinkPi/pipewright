package aptcacherng

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/huangchengsir/pipewright/internal/target"
)

// apt 代理片段下发常量。整文件由服务端生成覆盖(覆盖前备份);明文态写 http 代理地址,
// TLS 态写 https 代理地址(证书公共可信,客户端 apt 严格校验通过即零配置直连)。
// apt 读配置即生效,无需重启任何服务。
const (
	proxyConfPath  = "/etc/apt/apt.conf.d/95pipewright-proxy"
	applyTimeout   = 60 * time.Second // 单目标全流程(写文件 + 验证)
	verifyAttempts = 3                // apt-config dump 轮询次数(apt 无重启,间隔短)
	verifyInterval = 1 * time.Second
	// LocalTargetID 是「控制机本机」目标的固定标识(httpapi/前端约定;不走 SSH)。
	LocalTargetID = "local"
)

// proxyURL 返回写入代理片段的地址值(TLS 态为 https,明文态为 http;含 host:port)。
func proxyURL(cfg *Config) string {
	scheme := "http"
	if cfg.TLSEnabled() {
		scheme = "https"
	}
	return scheme + "://" + cfg.Addr()
}

// proxyConfContent 生成目标机代理片段完整内容(整文件覆盖)。http/https 两类源均指向
// 本缓存代理(https 源经 CONNECT 隧道透传,是 apt-cacher-ng 标准用法)。
func proxyConfContent(cfg *Config) []byte {
	url := proxyURL(cfg)
	return []byte("# 由 Pipewright 自动生成,手动修改会在下次下发时被覆盖。\n" +
		`Acquire::http::Proxy "` + url + `";` + "\n" +
		`Acquire::https::Proxy "` + url + `";` + "\n")
}

// ProxyApplyResult 是单台机器的下发结果(ok=false 时 error 人读,绝无凭据)。
type ProxyApplyResult struct {
	TargetID   string `json:"targetId"`
	TargetName string `json:"targetName"`
	IsLocal    bool   `json:"isLocal"`
	OK         bool   `json:"ok"`
	BackupPath string `json:"backupPath"`
	Error      string `json:"error"`
	DurationMs int64  `json:"durationMs"`
}

// remoteMachine 把 target.Service(SSH)适配成 MachineRunner(按 serverID)。
type remoteMachine struct {
	svc target.Service
	id  string
}

func (m remoteMachine) Exec(ctx context.Context, cmd []string) (string, string, int, error) {
	res, err := m.svc.Exec(ctx, m.id, cmd)
	if err != nil {
		return "", "", -1, err
	}
	return res.Stdout, res.Stderr, res.ExitCode, nil
}

func (m remoteMachine) Upload(ctx context.Context, content io.Reader, remotePath string) error {
	return m.svc.Upload(ctx, m.id, content, remotePath)
}

// localMachine 把控制机本机(LocalRunner + 文件系统)适配成 MachineRunner。
type localMachine struct {
	runner LocalRunner
}

func (m localMachine) Exec(ctx context.Context, cmd []string) (string, string, int, error) {
	if len(cmd) == 0 {
		return "", "", -1, errors.New("empty command")
	}
	return m.runner.Run(ctx, cmd[0], cmd[1:], "")
}

func (m localMachine) Upload(_ context.Context, content io.Reader, remotePath string) error {
	if err := os.MkdirAll(filepath.Dir(remotePath), 0o755); err != nil {
		return err
	}
	b, err := io.ReadAll(content)
	if err != nil {
		return err
	}
	// 0644(apt 配置全局可读;与远端路径一致)。
	return os.WriteFile(remotePath, b, 0o644)
}

// proxyTarget 是一个待下发目标(runner + 展示信息)。
type proxyTarget struct {
	id      string
	name    string
	isLocal bool
	runner  MachineRunner
}

// ApplyProxy 把生成的 apt 代理片段下发到**显式指定**的目标(唯一触发方式:用户在设置页
// 勾选并确认;本方法绝不自行扩大目标范围)。逐台执行:探测 apt-get → 备份原片段 → 写临时
// 文件 → 原子替换 → apt-config dump 轮询验证生效。批量并发上限 opts.ApplyConcurrency;
// 单目标超时 applyTimeout。未 enabled → ErrDisabled(片段内容依赖本缓存地址)。
func (h *Hub) ApplyProxy(ctx context.Context, serverIDs []string, includeLocal bool) ([]ProxyApplyResult, error) {
	cfg, err := h.Get(ctx)
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, ErrDisabled
	}

	targets, err := h.resolveTargets(ctx, serverIDs, includeLocal)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	results := make([]ProxyApplyResult, len(targets))
	sem := make(chan struct{}, h.opts.ApplyConcurrency)
	var wg sync.WaitGroup
	for i, tgt := range targets {
		wg.Add(1)
		go func(i int, tgt proxyTarget) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = h.applyOne(ctx, tgt, cfg, now)
		}(i, tgt)
	}
	wg.Wait()
	return results, nil
}

// resolveTargets 把请求里的显式目标(id 列表 + 是否含本机)解析为待执行目标。
// 服务器不存在 → 该目标结果直接置失败(不阻断其余);本机无需登记。重复 id 去重(保序)。
func (h *Hub) resolveTargets(ctx context.Context, serverIDs []string, includeLocal bool) ([]proxyTarget, error) {
	var targets []proxyTarget
	if includeLocal {
		targets = append(targets, proxyTarget{
			id:      LocalTargetID,
			name:    "控制机本机",
			isLocal: true,
			runner:  h.opts.LocalMachine,
		})
	}
	seen := map[string]bool{LocalTargetID: true}
	for _, id := range serverIDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if h.targetSvc == nil {
			// 未装配 SSH 通道(纯本机部署场景):目标如实置失败,不静默吞掉。
			targets = append(targets, proxyTarget{id: id, name: id})
			continue
		}
		srv, err := h.targetSvc.Get(ctx, id)
		if err != nil {
			// 服务器不存在:仍占一个结果位(报告失败),不静默吞掉。
			targets = append(targets, proxyTarget{id: id, name: id})
			continue
		}
		targets = append(targets, proxyTarget{
			id:     id,
			name:   srv.Name,
			runner: remoteMachine{svc: h.targetSvc, id: id},
		})
	}
	return targets, nil
}

// applyOne 对单台机器执行完整下发流程。所有失败都落在结果里(人读 error),绝不 panic /
// 绝不让单机失败影响并发中的其他机器。命令一律 array;复合命令经 `sh -c` 且脚本为包内
// 常量、动态值走位置参数(AC-SEC-02,防注入)。
func (h *Hub) applyOne(parent context.Context, tgt proxyTarget, cfg *Config, now time.Time) ProxyApplyResult {
	res := ProxyApplyResult{TargetID: tgt.id, TargetName: tgt.name, IsLocal: tgt.isLocal}
	start := time.Now()
	defer func() { res.DurationMs = time.Since(start).Milliseconds() }()

	if tgt.runner == nil {
		res.Error = "目标不可用(服务器不存在或未装配)"
		return res
	}
	ctx, cancel := context.WithTimeout(parent, applyTimeout)
	defer cancel()

	// 1. 探测 apt 能力:无 apt-get 的机器(RHEL/Alpine 等)如实报告,不覆盖任何文件。
	if out, _, code, err := tgt.runner.Exec(ctx, []string{"sh", "-c", "command -v apt-get >/dev/null 2>&1"}); err != nil || code != 0 {
		res.Error = humanMachineError(err, out)
		return res
	}

	// 2. 备份现有片段(不存在则跳过;备份路径回显,便于人工回滚)。
	backupPath := proxyConfPath + ".bak.pipewright." + now.Format("20060102T150405Z")
	if _, _, code, err := tgt.runner.Exec(ctx, []string{"sh", "-c",
		`test -f "$0" && cp -a "$0" "$1" || true`, proxyConfPath, backupPath}); err != nil || code != 0 {
		res.Error = "备份现有代理配置失败"
		return res
	}
	res.BackupPath = backupPath

	// 3. 写临时文件(先落临时名,原子 mv 替换;片段 0644 全局可读,无校验关口——
	//    apt 配置无 --validate 类命令,正确性由末步 apt-config dump 验证兜底)。
	tmpPath := proxyConfPath + ".tmp.pipewright"
	if uerr := tgt.runner.Upload(ctx, bytes.NewReader(proxyConfContent(cfg)), tmpPath); uerr != nil {
		res.Error = "写入临时文件失败:" + humanMachineError(uerr, "")
		return res
	}

	// 4. 原子替换正式路径(同目录 mv)。旧文件存在时 mv 即覆盖,无需分形态脚本。
	if _, _, code, err := tgt.runner.Exec(ctx, []string{"mv", "-f", tmpPath, proxyConfPath}); err != nil || code != 0 {
		res.Error = "替换代理配置失败"
		return res
	}

	// 5. 轮询验证 apt-config 已解析出指向本缓存的代理值(片段语法错误会被 apt 静默跳过,
	//    这里用生效值兜底确认)。比对地址 token(host:port)而非整行赋值——apt-config dump
	//    是树状输出且可能对 URL 做规范化(补斜杠),token 比对不受这些差异影响。
	want := cfg.Addr()
	for attempt := 0; attempt < verifyAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				res.Error = "验证超时(apt-config 未返回)"
				return res
			case <-time.After(verifyInterval):
			}
		}
		out, _, code, err := tgt.runner.Exec(ctx, []string{"apt-config", "dump"})
		if err != nil || code != 0 {
			continue // apt-config 暂不可用,继续轮询
		}
		if aptDumpPointsAt(out, want) {
			res.OK = true
			return res
		}
		res.Error = "代理配置已写入,但 apt-config 未指向本缓存地址 " + want + "(片段可能被 apt 忽略)"
		return res
	}
	res.Error = "apt-config 未就绪,代理配置生效情况未能确认"
	return res
}

// aptDumpPointsAt 报告 apt-config dump 输出是否含目标缓存地址 token(host:port;树状/
// 规范化差异均不影响该子串存在性)。
func aptDumpPointsAt(out, addr string) bool {
	return addr != "" && strings.Contains(out, addr)
}

// ProxyInspect 是单机 apt 代理现状巡检结果(只读)。
type ProxyInspect struct {
	TargetID   string `json:"targetId"`
	TargetName string `json:"targetName"`
	IsLocal    bool   `json:"isLocal"`
	ProxyHTTP  string `json:"proxyHttp"`  // apt-config 实际生效的 http 代理值
	ProxyHTTPS string `json:"proxyHttps"` // apt-config 实际生效的 https 代理值
	ConfFile   string `json:"confFile"`   // 片段文件原文(截断)
	Error      string `json:"error"`
}

// InspectProxy 只读巡检某机的 apt 代理现状(serverID="local" 为控制机本机)。
func (h *Hub) InspectProxy(ctx context.Context, serverID string) (*ProxyInspect, error) {
	ins := &ProxyInspect{TargetID: serverID}
	var runner MachineRunner
	switch {
	case serverID == LocalTargetID:
		runner = h.opts.LocalMachine
		ins.TargetName = "控制机本机"
		ins.IsLocal = true
	case h.targetSvc != nil:
		srv, err := h.targetSvc.Get(ctx, serverID)
		if err != nil {
			return nil, err
		}
		ins.TargetName = srv.Name
		runner = remoteMachine{svc: h.targetSvc, id: serverID}
	default:
		return nil, target.ErrNotFound
	}

	if _, _, code, err := runner.Exec(ctx, []string{"apt-config", "dump"}); err != nil || code != 0 {
		ins.Error = "读取 apt 配置失败(非 Debian 系或未安装 apt-config)"
	} else {
		ins.ProxyHTTP = aptProxyValue(ctx, runner, "Acquire::http::Proxy")
		ins.ProxyHTTPS = aptProxyValue(ctx, runner, "Acquire::https::Proxy")
	}
	if out, _, code, err := runner.Exec(ctx, []string{"cat", proxyConfPath}); err == nil && code == 0 {
		ins.ConfFile = tail(strings.TrimSpace(out), 4096)
	}
	return ins, nil
}

// aptProxyValue 按精确键读取 apt 生效代理值:apt-config shell 把指定键导出为 shell 赋值
// (/f 使键缺失时也输出空值),eval 后打印变量——规避 apt-config dump 树状输出里 http/https
// 下同名 Proxy 叶子的归属歧义。key 为包内常量(非用户输入),嵌入 sh -c 常量脚本安全(AC-SEC-02)。
func aptProxyValue(ctx context.Context, runner MachineRunner, key string) string {
	script := `eval "$(apt-config shell PWVAL ` + key + `/f 2>/dev/null)"; printf '%s' "${PWVAL:-}"`
	out, _, code, err := runner.Exec(ctx, []string{"sh", "-c", script})
	if err != nil || code != 0 {
		return ""
	}
	return strings.TrimSpace(out)
}

// humanMachineError 把机器执行错误映射为人读文案(SSH 层错误绝无凭据;out 为命令 stderr/stdout 尾部)。
func humanMachineError(err error, out string) string {
	if err == nil {
		msg := strings.TrimSpace(out)
		if msg == "" {
			return "机器上没有 apt-get(非 Debian/Ubuntu 系),无法下发 apt 代理配置"
		}
		return tail(msg, 256)
	}
	switch {
	case errors.Is(err, target.ErrAuth):
		return "SSH 认证失败:密钥或口令无效"
	case errors.Is(err, target.ErrUnreachable):
		return "无法连接服务器:主机不可达或超时"
	case errors.Is(err, target.ErrInvalidCredential):
		return "凭据不是可用的 SSH 私钥或口令"
	default:
		return "命令执行失败:" + tail(err.Error(), 256)
	}
}
