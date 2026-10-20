// detect.go 汇总本机执行前提的只读探测(best-effort,不动作、不报错;由 UI 展示人话
// 原因,由 preconditions 在动作前强校验)。
package firewall

import (
	"context"
	"strings"
)

// DetectInfo 是一次探测快照。
type DetectInfo struct {
	Linux       bool     // 运行平台是 Linux(功能唯一支持形态)
	InContainer bool     // 平台跑在容器里(容器内 iptables 管不到宿主)
	Iptables    string   // v4 二进制路径("" = 未找到)
	Ip6tables   string   // v6 二进制路径("" = 未找到;缺失仅降级不阻断)
	DockerUser  bool     // DOCKER-USER 链在(容器发布端口可限制)
	IsRoot      bool     // 平台运行用户是 root
	SudoOk      bool     // 免密 sudo 可用
	Notes       []string // 人话提示(探测到的降级项)
}

// Supported 汇报动作前提是否齐备(探测快照的便捷判定)。
func (d *DetectInfo) Supported() bool {
	return d != nil && d.Linux && !d.InContainer && d.Iptables != "" && (d.IsRoot || d.SudoOk)
}

// Reason 返回不支持的人话原因(Supported 时为空)。
func (d *DetectInfo) Reason() string {
	switch {
	case d == nil:
		return "探测不可用"
	case !d.Linux:
		return "防火墙限制仅支持 Linux 宿主机"
	case d.InContainer:
		return "容器部署形态下无法管理宿主防火墙,请在宿主机上直接配置(或改用宿主网络形态部署)"
	case d.Iptables == "":
		return "本机未找到 iptables(请安装 iptables 或确认 /usr/sbin 在可执行路径)"
	case !d.IsRoot && !d.SudoOk:
		return "平台运行用户非 root 且免密 sudo 不可用:请以 root 运行平台,或为该用户配置免密 sudo"
	}
	return ""
}

// detect 执行一轮探测(全 best-effort:单项失败不阻断其余)。
func (s *service) detect(ctx context.Context) *DetectInfo {
	d := &DetectInfo{Linux: hostGOOS == "linux", InContainer: inContainerMarker()}
	if !d.Linux || d.InContainer {
		return d // 其余探测在容器/非 Linux 形态下无意义
	}
	d.Iptables = resolveBinary(ctx, s.host, iptablesCandidates)
	d.Ip6tables = resolveBinary(ctx, s.host, ip6tablesCandidates)
	if out, _, code, err := s.host.Run(ctx, "id", []string{"-u"}); err == nil && code == 0 {
		d.IsRoot = strings.TrimSpace(out) == "0"
	}
	if !d.IsRoot {
		d.SudoOk = ranOK(ctx, s.host, "sudo", "-n", "true")
	}
	p := buildPrivilege(ctx, s.host)
	if d.Iptables != "" && (d.IsRoot || d.SudoOk) {
		d.DockerUser = dockerUserChainExists(ctx, s.host, p, d.Iptables)
	}
	if d.Iptables != "" && d.Ip6tables == "" {
		d.Notes = append(d.Notes, "未找到 ip6tables,IPv6 入口不会受限")
	}
	if d.Iptables != "" && (d.IsRoot || d.SudoOk) && !d.DockerUser {
		d.Notes = append(d.Notes, "DOCKER-USER 链不存在(无 Docker 或未启动),容器发布端口不会受限")
	}
	return d
}

// ranOK 报告一条命令是否以退出码 0 跑完(启动失败按否)。
func ranOK(ctx context.Context, h Host, name string, args ...string) bool {
	_, _, code, err := h.Run(ctx, name, args)
	return err == nil && code == 0
}
