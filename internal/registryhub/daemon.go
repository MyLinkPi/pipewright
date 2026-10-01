package registryhub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/huangchengsir/pipewright/internal/target"
)

// daemon.json 下发常量。整文件由服务端生成覆盖(覆盖前备份);生成内容仅两类键:
// registry-mirrors(指向本平台缓存 registry)+ insecure-registries(明文 HTTP 所需)。
const (
	daemonJSONPath = "/etc/docker/daemon.json"
	applyTimeout   = 120 * time.Second // 单目标全流程(含重启 + 轮询验证)
	verifyAttempts = 5                 // 重启后 docker info 轮询次数
	verifyInterval = 2 * time.Second
	// LocalTargetID 是「控制机本机」目标的固定标识(httpapi/前端约定;不走 SSH)。
	LocalTargetID = "local"
)

// mirrorURL 返回写入 daemon.json 的 registry-mirrors 值(明文 HTTP,指向缓存 registry)。
func mirrorURL(cfg *Config) string { return "http://" + cfg.CacheAddr() }

// daemonJSONContent 生成目标机 /etc/docker/daemon.json 的完整内容(整文件覆盖)。
// 生产/部署/构建机对内置 registry 的全部依赖(基础镜像加速 + 制品明文拉推)一次配齐。
func daemonJSONContent(cfg *Config) []byte {
	doc := map[string]any{
		"registry-mirrors":    []string{mirrorURL(cfg)},
		"insecure-registries": []string{cfg.ArtifactAddr(), cfg.CacheAddr()},
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return []byte("{}\n")
	}
	return append(b, '\n')
}

// DaemonApplyResult 是单台机器的下发结果(ok=false 时 error 人读,绝无凭据)。
type DaemonApplyResult struct {
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
	// 0644 + 随后统一 chmod 600(与远端 sh 路径一致;Windows 开发机上退化为默认权限)。
	return os.WriteFile(remotePath, b, 0o644)
}

// daemonTarget 是一个待下发目标(runner + 展示信息)。
type daemonTarget struct {
	id      string
	name    string
	isLocal bool
	runner  MachineRunner
}

// ApplyDaemon 把生成的 daemon.json 下发到**显式指定**的目标(唯一触发方式:用户在设置页
// 勾选并确认;本方法绝不自行扩大目标范围)。逐台执行:探测 docker → 备份原文件 → 写临时
// 文件 → dockerd --validate(可用时)→ 原子替换 → systemctl restart docker → 轮询
// docker info 验证镜像源生效。批量并发上限 opts.ApplyConcurrency;单目标超时 applyTimeout。
// 未 enabled → ErrDisabled(镜像源内容依赖本 registry 地址)。
func (h *Hub) ApplyDaemon(ctx context.Context, serverIDs []string, includeLocal bool) ([]DaemonApplyResult, error) {
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
	results := make([]DaemonApplyResult, len(targets))
	sem := make(chan struct{}, h.opts.ApplyConcurrency)
	var wg sync.WaitGroup
	for i, tgt := range targets {
		wg.Add(1)
		go func(i int, tgt daemonTarget) {
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
func (h *Hub) resolveTargets(ctx context.Context, serverIDs []string, includeLocal bool) ([]daemonTarget, error) {
	var targets []daemonTarget
	if includeLocal {
		targets = append(targets, daemonTarget{
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
			targets = append(targets, daemonTarget{id: id, name: id})
			continue
		}
		srv, err := h.targetSvc.Get(ctx, id)
		if err != nil {
			// 服务器不存在:仍占一个结果位(报告失败),不静默吞掉。
			targets = append(targets, daemonTarget{id: id, name: id})
			continue
		}
		targets = append(targets, daemonTarget{
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
func (h *Hub) applyOne(parent context.Context, tgt daemonTarget, cfg *Config, now time.Time) DaemonApplyResult {
	res := DaemonApplyResult{TargetID: tgt.id, TargetName: tgt.name, IsLocal: tgt.isLocal}
	start := time.Now()
	defer func() { res.DurationMs = time.Since(start).Milliseconds() }()

	if tgt.runner == nil {
		res.Error = "目标不可用(服务器不存在或未装配)"
		return res
	}
	ctx, cancel := context.WithTimeout(parent, applyTimeout)
	defer cancel()

	// 1. 探测容器运行时(无 docker/dockerd 的机器如实报告,不覆盖任何文件)。
	if out, _, code, err := tgt.runner.Exec(ctx, []string{"sh", "-c", "command -v dockerd >/dev/null 2>&1 || command -v docker >/dev/null 2>&1"}); err != nil || code != 0 {
		res.Error = humanMachineError(err, out)
		return res
	}

	// 2. 备份现有 daemon.json(不存在则跳过;备份路径回显,便于人工回滚)。
	backupPath := daemonJSONPath + ".bak.pipewright." + now.Format("20060102T150405Z")
	if _, _, code, err := tgt.runner.Exec(ctx, []string{"sh", "-c",
		`test -f "$0" && cp -a "$0" "$1" || true`, daemonJSONPath, backupPath}); err != nil || code != 0 {
		res.Error = "备份现有 daemon.json 失败"
		return res
	}
	res.BackupPath = backupPath

	// 3. 写临时文件(先落临时名,校验通过再原子 mv,半成品绝不直接覆盖正式路径)。
	tmpPath := daemonJSONPath + ".tmp.pipewright"
	if uerr := tgt.runner.Upload(ctx, bytes.NewReader(daemonJSONContent(cfg)), tmpPath); uerr != nil {
		res.Error = "写入临时文件失败:" + humanMachineError(uerr, "")
		return res
	}
	if _, _, code, err := tgt.runner.Exec(ctx, []string{"chmod", "600", tmpPath}); err != nil || code != 0 {
		res.Error = "设置临时文件权限失败"
		return res
	}

	// 4. 先测后换:dockerd --validate(dockerd 不在/极老版本 → 127/-1,跳过校验不阻断;
	//    校验明确失败 → 中止,正式文件未被触碰)。
	if _, stderr, code, err := tgt.runner.Exec(ctx, []string{"dockerd", "--validate", "--config-file=" + tmpPath}); err == nil && code > 0 && code != 127 {
		res.Error = "daemon.json 校验未通过:" + tail(strings.TrimSpace(stderr), 512)
		_, _, _, _ = tgt.runner.Exec(ctx, []string{"rm", "-f", tmpPath})
		return res
	}

	// 5. 原子替换正式路径(同目录 mv)。
	if _, _, code, err := tgt.runner.Exec(ctx, []string{"sh", "-c", `mv -f "$0" "$1"`, tmpPath, daemonJSONPath}); err != nil || code != 0 {
		res.Error = "替换 daemon.json 失败"
		return res
	}

	// 6. 重启 docker 生效(重启会短暂中断该机容器;调用方 UI 已明确提示)。
	if _, stderr, code, err := tgt.runner.Exec(ctx, []string{"systemctl", "restart", "docker"}); err != nil || code != 0 {
		res.Error = "重启 docker 失败:" + tail(strings.TrimSpace(stderr), 512)
		return res
	}

	// 7. 轮询验证镜像源已生效(daemon 重启需要数秒就绪)。
	want := mirrorURL(cfg)
	for attempt := 0; attempt < verifyAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				res.Error = "验证超时(docker 仍在重启中)"
				return res
			case <-time.After(verifyInterval):
			}
		}
		out, _, code, err := tgt.runner.Exec(ctx, []string{"docker", "info", "--format", "{{json .RegistryConfig.Mirrors}}"})
		if err != nil || code != 0 {
			continue // daemon 未就绪,继续轮询
		}
		var mirrors []string
		if jerr := json.Unmarshal([]byte(strings.TrimSpace(out)), &mirrors); jerr == nil {
			for _, m := range mirrors {
				if strings.TrimRight(m, "/") == strings.TrimRight(want, "/") {
					res.OK = true
					return res
				}
			}
			res.Error = "docker 已重启,但镜像源未生效(镜像源列表:" + truncateJoin(mirrors, 256) + ")"
			return res
		}
	}
	res.Error = "docker 重启后未就绪,镜像源生效情况未能确认"
	return res
}

// DaemonInspect 是单机 daemon.json 现状巡检结果(只读)。
type DaemonInspect struct {
	TargetID   string   `json:"targetId"`
	TargetName string   `json:"targetName"`
	IsLocal    bool     `json:"isLocal"`
	Mirrors    []string `json:"mirrors"`    // docker info 实际生效的镜像源
	DaemonJSON string   `json:"daemonJson"` // /etc/docker/daemon.json 原文(截断)
	Error      string   `json:"error"`
}

// InspectDaemon 只读巡检某机的镜像源现状(serverId="local" 为控制机本机)。
func (h *Hub) InspectDaemon(ctx context.Context, serverID string) (*DaemonInspect, error) {
	ins := &DaemonInspect{TargetID: serverID, Mirrors: []string{}}
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

	if out, _, code, err := runner.Exec(ctx, []string{"docker", "info", "--format", "{{json .RegistryConfig.Mirrors}}"}); err != nil || code != 0 {
		ins.Error = "读取 docker 镜像源失败(未安装 docker 或未运行)"
	} else if jerr := json.Unmarshal([]byte(strings.TrimSpace(out)), &ins.Mirrors); jerr != nil {
		ins.Mirrors = []string{}
		ins.Error = "docker 镜像源输出不可解析"
	}
	if out, _, code, err := runner.Exec(ctx, []string{"cat", daemonJSONPath}); err == nil && code == 0 {
		ins.DaemonJSON = tail(strings.TrimSpace(out), 4096)
	}
	return ins, nil
}

// humanMachineError 把机器执行错误映射为人读文案(SSH 层错误绝无凭据;out 为命令 stderr/stdout 尾部)。
func humanMachineError(err error, out string) string {
	if err == nil {
		msg := strings.TrimSpace(out)
		if msg == "" {
			return "机器上未检测到 Docker(docker/dockerd 均不存在)"
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

// truncateJoin 把字符串列表拼接并截断(供人读错误)。
func truncateJoin(items []string, n int) string {
	s := strings.Join(items, ",")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
