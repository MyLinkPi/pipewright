package secscan

import (
	"context"
	"strings"
	"time"
)

// 代扫路径:目标机无 docker 时,主机包 CVE 扫描改由已登记 Trivy Server 的宿主机代跑。
//
// 原理:trivy rootfs 只读包管理元数据(dpkg status / rpmdb / apk db 等),不需要完整
// 根文件系统。于是把元数据打成 tar 经面板中继——目标机 ExecStream 流出 → Server 宿主机
// ExecWithStdin 喂进代扫容器,容器内解包后 trivy rootfs 扫描。目标机全程只需 SSH + tar,
// 无需 docker(目标机上的容器镜像扫描因此跳过:无 docker 即无运行中 docker 容器)。
//
// 纪律不变:argv 数组化;脚本串是常量,目标机数据只走 stdin;--server/--token 经容器
// 环境变量注入(TRIVY_SERVER/TRIVY_TOKEN),不进脚本串。

// trivyRemoteMetaPaths 是代扫模式下从目标机打包的包管理元数据(trivy rootfs 分析所需
// 最小集合;各发行版按需命中,ls -d 探测后不存在的直接跳过)。
var trivyRemoteMetaPaths = []string{
	"/etc/os-release", "/usr/lib/os-release", // OS 识别
	"/etc/redhat-release", "/etc/debian_version", "/etc/alpine-release", // 老版本兜底
	"/var/lib/dpkg/status", "/var/lib/dpkg/status.d", // Debian/Ubuntu
	"/lib/apk/db/installed", // Alpine
	"/var/lib/rpm", "/usr/lib/sysimage/rpm", // RHEL 系(Fedora 36+ 在 sysimage)
	"/var/lib/pacman/local", // Arch
}

// runTrivyViaServerHost 经 Trivy Server 宿主机代跑主机包扫描。已接入中心服务(有接入点)
// 但宿主机未知(手工登记无 ServerID)→ 人话指引;未接入 → 原提示。
func (s *service) runTrivyViaServerHost(ctx context.Context, run *Run, host, serverURL, token string) (*ReportDoc, string) {
	if serverURL == "" {
		return nil, dockerUnavailableMsg
	}
	inst, err := s.GetScanner(ctx, run.ScannerID)
	if err != nil || inst.ServerID == "" {
		return nil, dockerUnavailableManualServerMsg
	}

	// ① 探测目标机上实际存在的元数据路径(ls 对不存在的报 stderr 但 stdout 照常输出,
	// 不看退出码)。全部缺席则无包信息可扫。
	probe, err := s.runner.Exec(ctx, run.ServerID, append([]string{"ls", "-d"}, trivyRemoteMetaPaths...))
	if err != nil {
		return nil, "SSH 执行失败:" + truncateRunes(err.Error(), 256)
	}
	paths := make([]string, 0, len(trivyRemoteMetaPaths))
	for _, line := range strings.Split(probe.Stdout, "\n") {
		if p := strings.TrimSpace(line); strings.HasPrefix(p, "/") {
			paths = append(paths, strings.TrimPrefix(p, "/"))
		}
	}
	if len(paths) == 0 {
		return nil, "目标机无 docker 且未发现已知包管理元数据,无法经 Trivy Server 宿主机代扫"
	}

	// ② 目标机打元数据 tar(-C / 用相对路径,免「Removing leading /」歧义)→ 面板中继
	// → Server 宿主机代扫容器 stdin。
	tarCmd := append([]string{"tar", "-C", "/", "-cf", "-"}, paths...)
	tarc, terr := s.runner.ExecStream(ctx, run.ServerID, tarCmd)
	if terr != nil {
		return nil, "流式读取目标机元数据失败:" + truncateRunes(terr.Error(), 256)
	}
	defer func() { _ = tarc.Close() }()

	// ③ 容器内解包到 /scan 后 trivy rootfs(脚本为常量,数据只走 stdin)。
	// 镜像 ENTRYPOINT 恒为 trivy(常规路径的 `docker run … trivy镜像 rootfs` 即依赖此),
	// 跑 shell 必须显式 --entrypoint sh,否则 sh 会作为 trivy 子命令报 unknown command。
	script := "mkdir -p /scan && tar -xf - -C /scan && trivy rootfs -q --scanners vuln --format json /scan"
	scanCmd := []string{"docker", "run", "--rm", "-i", "--entrypoint", "sh", "-e", "TRIVY_SERVER=" + serverURL}
	if token != "" {
		scanCmd = append(scanCmd, "-e", "TRIVY_TOKEN="+token)
	}
	scanCmd = append(scanCmd, s.trivyImage, "-c", script)
	res, rerr := s.runner.ExecWithStdin(ctx, inst.ServerID, scanCmd, tarc)
	if rerr != nil {
		return nil, "经 Trivy Server 宿主机(" + inst.Name + ")代跑失败:" + truncateRunes(rerr.Error(), 256)
	}
	if dockerUnavailable(res, nil) {
		return nil, "Trivy Server 宿主机 docker 不可用,无法代跑主机包扫描:" + tailClip(res.Stderr, 160)
	}
	if res.ExitCode != 0 {
		return nil, "代扫 trivy 失败:" + tailClip(res.Stderr, 240)
	}
	findings, perr := TrivyFindings([]byte(res.Stdout), "host")
	if perr != nil {
		return nil, "代扫结果解析失败:" + truncateRunes(perr.Error(), 120)
	}
	SortFindings(findings)
	doc := &ReportDoc{
		ScanType: ScanTypeTrivy, GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Host: host, ServerName: run.ServerName, Findings: findings,
		Notes: []string{"目标机无 docker:主机包扫描已经 Trivy Server 宿主机(" + inst.Name + ")代跑;运行中容器镜像扫描已跳过"},
	}
	return doc, ""
}
