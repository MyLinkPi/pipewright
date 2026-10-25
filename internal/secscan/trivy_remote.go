package secscan

import (
	"context"
	"fmt"
	"io"
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
// 环境变量注入(TRIVY_SERVER/TRIVY_TOKEN),不进脚本串、不进日志。
// 所有出口都返回携带执行日志的 doc(失败也一样):「扫不出结果」类问题靠原始输出诊断。

// trivyRemoteMetaPaths 是代扫模式下从目标机打包的包管理元数据(trivy rootfs 分析所需
// 最小集合;各发行版按需命中,ls -d 探测后不存在的直接跳过)。
var trivyRemoteMetaPaths = []string{
	"/etc/os-release", "/usr/lib/os-release", // OS 识别
	"/etc/lsb-release",                                                  // trivy 识别 Ubuntu 的唯一依据(实测 0.76;新版 Ubuntu 缺失,容器内补写)
	"/etc/redhat-release", "/etc/debian_version", "/etc/alpine-release", // 老版本兜底
	"/var/lib/dpkg/status", "/var/lib/dpkg/status.d", // Debian/Ubuntu
	"/lib/apk/db/installed",                 // Alpine
	"/var/lib/rpm", "/usr/lib/sysimage/rpm", // RHEL 系(Fedora 36+ 在 sysimage)
	"/var/lib/pacman/local", // Arch
}

// trivyRemoteScanScript 是代扫容器内执行的复合脚本(常量;目标机数据只走 stdin,绝不
// 拼进脚本):
//  1. 解包元数据到 /scan,并把清单打到 stderr(零结果时日志可见元数据是否进容器);
//  2. trivy 只认 /etc/lsb-release 识别 Ubuntu(实测 0.76:os-release 不参与 debian 家族
//     判定,只有 /etc/debian_version → 新版 Ubuntu 被误判 debian forky/sid,漏洞库无数据
//     → 零漏洞)。新版 Ubuntu 不带该文件,从 os-release 用 sed 提取 ID/VERSION_ID 补写
//     (不 source 目标机文件,防把目标机数据当代码执行);
//  3. trivy rootfs 扫描,漏洞匹配走 --server(经 TRIVY_SERVER/TRIVY_TOKEN 环境变量注入)。
const trivyRemoteScanScript = `set -e
mkdir -p /scan
tar -xf - -C /scan
ls -laR /scan >&2
if [ ! -f /scan/etc/lsb-release ] && [ -f /scan/etc/os-release ]; then
  id=$(sed -n 's/^ID=//p' /scan/etc/os-release | head -n1 | tr -d '"')
  ver=$(sed -n 's/^VERSION_ID=//p' /scan/etc/os-release | head -n1 | tr -d '" ')
  if [ "$id" = ubuntu ] && [ -n "$ver" ]; then
    printf 'DISTRIB_ID=Ubuntu\nDISTRIB_RELEASE=%s\n' "$ver" > /scan/etc/lsb-release
  fi
fi
trivy rootfs -q --scanners vuln --format json /scan`

// countingReader 统计经面板中继的字节数(诊断「容器扫了空目录」类问题)。
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// runTrivyViaServerHost 经 Trivy Server 宿主机代跑主机包扫描。已接入中心服务(有接入点)
// 但宿主机未知(手工登记无 ServerID)→ 人话指引;未接入 → 原提示。无论成败都返回带
// 执行日志的 doc(零 findings + errMsg 由 executeAsync 判为 failed)。
func (s *service) runTrivyViaServerHost(ctx context.Context, run *Run, host, serverURL, token string) (*ReportDoc, string) {
	if serverURL == "" {
		return failDoc(run, host, ScanTypeTrivy, nil), dockerUnavailableMsg
	}
	inst, err := s.GetScanner(ctx, run.ScannerID)
	if err != nil || inst.ServerID == "" {
		return failDoc(run, host, ScanTypeTrivy, nil), dockerUnavailableManualServerMsg
	}
	logs := make([]StepLog, 0, 4)

	// ① 探测目标机上实际存在的元数据路径(ls 对不存在的报 stderr 但 stdout 照常输出,
	// 不看退出码)。全部缺席则无包信息可扫。
	probe, perr := s.runner.Exec(ctx, run.ServerID, append([]string{"ls", "-d"}, trivyRemoteMetaPaths...))
	logs = append(logs, stepLog("元数据探测(ls -d,目标机)", probe, perr))
	if perr != nil {
		return failDoc(run, host, ScanTypeTrivy, logs), "SSH 执行失败:" + truncateRunes(perr.Error(), 256)
	}
	paths := make([]string, 0, len(trivyRemoteMetaPaths))
	for _, line := range strings.Split(probe.Stdout, "\n") {
		if p := strings.TrimSpace(line); strings.HasPrefix(p, "/") {
			paths = append(paths, strings.TrimPrefix(p, "/"))
		}
	}
	if len(paths) == 0 {
		return failDoc(run, host, ScanTypeTrivy, logs), "目标机无 docker 且未发现已知包管理元数据,无法经 Trivy Server 宿主机代扫"
	}

	// ② 目标机打元数据 tar(-C / 用相对路径,免「Removing leading /」歧义)→ 面板中继
	// → Server 宿主机代扫容器 stdin。
	tarCmd := append([]string{"tar", "-C", "/", "-cf", "-"}, paths...)
	tarc, terr := s.runner.ExecStream(ctx, run.ServerID, tarCmd)
	if terr != nil {
		return failDoc(run, host, ScanTypeTrivy, logs), "流式读取目标机元数据失败:" + truncateRunes(terr.Error(), 256)
	}
	defer func() { _ = tarc.Close() }()
	relay := &countingReader{r: tarc}

	// ③ 容器内解包到 /scan 后 trivy rootfs。trivy 镜像的 ENTRYPOINT 是 trivy 本体,须经
	// --entrypoint sh 才能跑复合脚本(本地路径不受影响:args 直接落在 entrypoint 后面,
	// 恰好就是 trivy 子命令)。
	scanCmd := []string{"docker", "run", "--rm", "-i", "--entrypoint", "sh", "-e", "TRIVY_SERVER=" + serverURL}
	if token != "" {
		scanCmd = append(scanCmd, "-e", "TRIVY_TOKEN="+token)
	}
	scanCmd = append(scanCmd, s.trivyImage, "-c", trivyRemoteScanScript)
	res, rerr := s.runner.ExecWithStdin(ctx, inst.ServerID, scanCmd, relay)
	logs = append(logs, StepLog{Step: "元数据 tar 中继(目标机 → 面板 → Server 宿主机)", ExitCode: 0,
		Stdout: fmt.Sprintf("共中继 %d 字节", relay.n)})
	logs = append(logs, stepLog("代扫容器(解包 + trivy rootfs /scan)", res, rerr))
	if rerr != nil {
		return failDoc(run, host, ScanTypeTrivy, logs), "经 Trivy Server 宿主机(" + inst.Name + ")代跑失败:" + truncateRunes(rerr.Error(), 256)
	}
	if dockerUnavailable(res, nil) {
		return failDoc(run, host, ScanTypeTrivy, logs), "Trivy Server 宿主机 docker 不可用,无法代跑主机包扫描:" + tailClip(res.Stderr, 160)
	}
	if res.ExitCode != 0 {
		return failDoc(run, host, ScanTypeTrivy, logs), "代扫 trivy 失败:" + tailClip(res.Stderr, 240)
	}
	findings, prsErr := TrivyFindings([]byte(res.Stdout), "host")
	if prsErr != nil {
		return failDoc(run, host, ScanTypeTrivy, logs), "代扫结果解析失败:" + truncateRunes(prsErr.Error(), 120)
	}
	SortFindings(findings)
	doc := &ReportDoc{
		ScanType: ScanTypeTrivy, GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Host: host, ServerName: run.ServerName, Findings: findings, Logs: logs,
		Notes: []string{"目标机无 docker:主机包扫描已经 Trivy Server 宿主机(" + inst.Name + ")代跑;运行中容器镜像扫描已跳过"},
	}
	return doc, ""
}
