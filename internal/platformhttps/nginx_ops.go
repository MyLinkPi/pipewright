package platformhttps

import (
	"context"
	"fmt"
	"strings"
)

// 临时落盘路径:三个文件收进平台运行用户私有的 700 目录(防多用户机上 chmod 前的可读窗口;
// 应用完成后整目录清理,备份文件一并删除)。
const (
	tmpDir        = "/tmp/pipewright-platform-https.d"
	tmpConfPath   = tmpDir + "/pipewright-platform.conf"
	tmpCertPath   = tmpDir + "/fullchain.pem"
	tmpKeyPath    = tmpDir + "/privkey.pem"
	backupConfFmt = tmpDir + "/pipewright-platform.conf.bak"
)

// privilege 描述本机的一次提权方式(apply/remove 全程复用同一判定):
//
//	root  → prefix nil,直接执行;
//	免密  → prefix ["sudo","-n"]。
type privilege struct {
	prefix []string
}

// runResult 是一条命令的运行结果(供 trimOut 取人话摘要)。
type runResult struct {
	stdout string
	stderr string
}

// execOK 在本机跑一条 array 命令并报告退出码是否为 0(启动错误原样返回)。
func execOK(ctx context.Context, h Host, cmd ...string) (runResult, bool, error) {
	stdout, stderr, code, err := h.Run(ctx, cmd[0], cmd[1:])
	if err != nil {
		return runResult{stdout, stderr}, false, err
	}
	return runResult{stdout, stderr}, code == 0, nil
}

// execPriv 跑一条提权命令(等同 execOK 加前缀)。
func execPriv(ctx context.Context, h Host, p privilege, cmd ...string) (runResult, bool, error) {
	return execOK(ctx, h, append(p.prefix, cmd...)...)
}

// ---------- 探测 ----------

// detectNginx 探测本机宿主 nginx / 提权能力 / conf.d include 情况(只读,全部 best-effort
// 除命令启动错误)。
func detectNginx(ctx context.Context, h Host) (*NginxDetect, error) {
	d := &NginxDetect{}

	// 1) nginx 是否在 PATH(nginx -v 输出在 stderr;启动失败 = 不在 PATH,按未安装处理)。
	stdout, stderr, code, err := h.Run(ctx, "nginx", []string{"-v"})
	if err == nil && code == 0 {
		d.Installed = true
		d.Version = parseNginxVersion(firstNonEmpty(stderr, stdout))
	}

	// 2) 提权能力:root 直接可写;非 root 探免密 sudo。
	uid, ok, err := execOK(ctx, h, "id", "-u")
	if err != nil {
		return nil, err
	}
	if ok && strings.TrimSpace(uid.stdout) == "0" {
		d.IsRoot = true
		d.SudoOk = true
	} else if _, sudoOk, err := execOK(ctx, h, "sudo", "-n", "true"); err == nil && sudoOk {
		d.SudoOk = true
	}

	// 3) 平台管理的 vhost 是否已在(曾应用过)。conf.d 通常全局可读,普通 test 即可。
	if _, ok, _ := execOK(ctx, h, "test", "-f", managedConfPath); ok {
		d.ManagedConf = true
	}

	// 4) nginx -T 是否 include conf.d(防配置写入后静默失效;best-effort)。
	// 带提权执行:非 root 裸跑 nginx -T 常因读不到 root 600 的证书/私钥而失败,
	// 会误报「conf.d 未加载」;无提权能力时退化为裸跑(与 Apply 前的探测语义一致)。
	var p privilege
	if d.SudoOk && !d.IsRoot {
		p = privilege{prefix: []string{"sudo", "-n"}}
	}
	if dump, ok, _ := execPriv(ctx, h, p, "nginx", "-T"); ok {
		if strings.Contains(dump.stdout, "/etc/nginx/conf.d") {
			d.ConfDIncluded = true
		}
	}
	return d, nil
}

// parseNginxVersion 从 `nginx version: nginx/1.24.0` 提取版本号。
func parseNginxVersion(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "nginx/"); i >= 0 {
		return strings.TrimSpace(strings.TrimSuffix(s[i+len("nginx/"):], "\n"))
	}
	return ""
}

// ---------- 应用 ----------

// applyPlatformConf 在本机落位平台 HTTPS 配置并热加载:
//
//	探测 nginx + 提权方式(root > 免密 sudo)→ 建 700 临时目录并写入三份文件(私钥再收紧
//	600)→ 安装证书与 conf.d vhost(先删陈旧备份再备份既有同名文件)→ nginx -t 校验
//	(失败回滚,活配置不动)→ nginx -T 验证 conf.d 真被 include(失败回滚)→ reload
//	(nginx -s reload,失败回退 systemctl reload nginx)→ 清理临时目录(含备份)。
func applyPlatformConf(ctx context.Context, h Host, domain, upstreamHost string,
	upstreamPort int, httpRedirect bool, certPEM, keyPEM string) error {

	// 1) nginx 就绪(启动失败 = 不在 PATH,按未安装处理)+ 提权方式(失败快、不动本机)。
	if _, _, code, err := h.Run(ctx, "nginx", []string{"-v"}); err != nil || code != 0 {
		return ErrNoNginx
	}
	p, err := buildPrivilege(ctx, h)
	if err != nil {
		return err
	}

	conf := renderPlatformConf(domain, upstreamHost, upstreamPort, httpRedirect)
	certDir := certDir(domain)
	backup := backupConfFmt

	// 2) 临时文件落盘:先建运行用户私有的 700 目录再写入(目录 700 兜底,写入与 chmod
	// 之间不存在同机其他用户可读私钥的窗口)。
	for _, cmd := range [][]string{
		{"mkdir", "-p", tmpDir},
		{"chmod", "700", tmpDir},
	} {
		if res, ok, err := execOK(ctx, h, cmd...); err != nil {
			return err
		} else if !ok {
			return fmt.Errorf("%w:创建临时目录失败:%s", ErrApply, trimOut(res))
		}
	}
	for _, f := range []struct {
		path string
		data string
	}{
		{tmpConfPath, conf},
		{tmpCertPath, certPEM},
		{tmpKeyPath, keyPEM},
	} {
		if err := h.WriteFile(f.path, []byte(f.data), 0o600); err != nil {
			return fmt.Errorf("%w:写临时文件失败:%s", ErrApply, err)
		}
	}
	defer func() { cleanupTmp(ctx, h, p) }()
	if _, ok, err := execOK(ctx, h, "chmod", "600", tmpKeyPath); err != nil || !ok {
		return fmt.Errorf("%w:收紧私钥临时文件权限失败", ErrApply)
	}

	// 3) 安装证书(conf.d 目录一并确保存在,防源码编译版无该目录)。
	for _, cmd := range [][]string{
		{"mkdir", "-p", certDir, "/etc/nginx/conf.d"},
		{"cp", tmpCertPath, certDir + "/fullchain.pem"},
		{"cp", tmpKeyPath, certDir + "/privkey.pem"},
		{"chmod", "600", certDir + "/privkey.pem"},
	} {
		if res, ok, err := execPriv(ctx, h, p, cmd...); err != nil {
			return err
		} else if !ok {
			return fmt.Errorf("%w:%s", ErrApply, trimOut(res))
		}
	}

	// 4) 备份既有平台 vhost(文件可能不存在,非零退出可容忍)→ 覆盖安装新配置。
	// 先删陈旧备份(上次运行被中断时可能残留),保证 rollbackConf 还原的只会是本次备份。
	_, _, _ = execPriv(ctx, h, p, "rm", "-f", backup)
	_, _, _ = execPriv(ctx, h, p, "cp", managedConfPath, backup)
	if res, ok, err := execPriv(ctx, h, p, "cp", tmpConfPath, managedConfPath); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("%w:%s", ErrApply, trimOut(res))
	}

	// 5) nginx -t(先测后换已就位:校验失败回滚到备份/摘除我们的文件,活配置语义不变)。
	if res, ok, err := execPriv(ctx, h, p, "nginx", "-t"); err != nil {
		return err
	} else if !ok {
		rollbackConf(ctx, h, p, backup)
		return fmt.Errorf("%w:nginx -t 校验失败:%s", ErrApply, trimOut(res))
	}

	// 6) nginx -T 验证 conf.d 真被 include(防部分手装 nginx 未 include conf.d,配置静默失效)。
	if res, ok, err := execPriv(ctx, h, p, "nginx", "-T"); err != nil {
		return err
	} else if !ok || !strings.Contains(res.stdout, confMarker) {
		rollbackConf(ctx, h, p, backup)
		return fmt.Errorf("%w:配置未被 nginx 加载(conf.d 未被 include),请在本机 nginx.conf 的 http{} 内加入 include /etc/nginx/conf.d/*.conf;", ErrApply)
	}

	// 7) 热加载:nginx -s reload → 失败回退 systemctl reload nginx(不自动 start)。
	if res, ok, err := execPriv(ctx, h, p, "nginx", "-s", "reload"); err != nil {
		return err
	} else if !ok {
		if res2, ok2, err2 := execPriv(ctx, h, p, "systemctl", "reload", "nginx"); err2 != nil || !ok2 {
			return fmt.Errorf("%w:配置已就位但热加载失败(nginx 可能未运行):%s",
				ErrApply, firstNonEmpty(trimOut(res), trimOut(res2)))
		}
	}
	return nil
}

// removePlatformConf 移除平台 vhost 与证书目录并热加载;本机无平台配置时为 no-op(幂等)。
func removePlatformConf(ctx context.Context, h Host, domain string) error {
	// 无平台配置 → 直接成功(不要求提权/不触 nginx)。
	if _, ok, err := execOK(ctx, h, "test", "-f", managedConfPath); err != nil {
		return err
	} else if !ok {
		return nil
	}
	p, err := buildPrivilege(ctx, h)
	if err != nil {
		return err
	}
	defer func() { cleanupTmp(ctx, h, p) }()

	for _, cmd := range [][]string{
		{"rm", "-f", managedConfPath},
		{"rm", "-rf", certDir(domain)},
		{"rmdir", certsBaseDir}, // 仅当空目录时成功(非零可容忍)
	} {
		if res, ok, err := execPriv(ctx, h, p, cmd...); err != nil {
			return err
		} else if !ok && cmd[0] != "rmdir" {
			return fmt.Errorf("%w:%s", ErrApply, trimOut(res))
		}
	}
	// 剩余配置校验 + 热加载(摘除平台 vhost 后立即生效)。
	if res, ok, err := execPriv(ctx, h, p, "nginx", "-t"); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("%w:摘除平台配置后 nginx -t 校验失败(本机其余配置自身有误):%s", ErrApply, trimOut(res))
	}
	if res, ok, err := execPriv(ctx, h, p, "nginx", "-s", "reload"); err != nil {
		return err
	} else if !ok {
		if _, ok2, err2 := execPriv(ctx, h, p, "systemctl", "reload", "nginx"); err2 != nil || !ok2 {
			return fmt.Errorf("%w:配置已摘除但热加载失败(nginx 可能未运行):%s", ErrApply, trimOut(res))
		}
	}
	return nil
}

// ---------- 内部 ----------

// buildPrivilege 判定本机提权方式:root 直接执行 > 免密 sudo。皆不可用报 ErrNoPrivilege
// 并给人话指引。绝不交互输密、绝不挂起等 TTY。
func buildPrivilege(ctx context.Context, h Host) (privilege, error) {
	res, ok, err := execOK(ctx, h, "id", "-u")
	if err != nil {
		return privilege{}, err
	}
	if ok && strings.TrimSpace(res.stdout) == "0" {
		return privilege{}, nil // root:直接执行
	}
	if _, ok, err := execOK(ctx, h, "sudo", "-n", "true"); err != nil {
		return privilege{}, err
	} else if ok {
		return privilege{prefix: []string{"sudo", "-n"}}, nil
	}
	return privilege{}, fmt.Errorf("%w:平台运行用户非 root 且无免密 sudo;请以 root 运行平台,或为该用户配置免密 sudo(如 /etc/sudoers.d/pipewright)", ErrNoPrivilege)
}

// rollbackConf 在校验失败时恢复活配置:有备份 → 还原;无备份(首次应用)→ 摘除我们的文件。
func rollbackConf(ctx context.Context, h Host, p privilege, backup string) {
	if _, ok, _ := execPriv(ctx, h, p, "test", "-f", backup); ok {
		_, _, _ = execPriv(ctx, h, p, "cp", backup, managedConfPath)
		return
	}
	_, _, _ = execPriv(ctx, h, p, "rm", "-f", managedConfPath)
}

// cleanupTmp 清理临时目录(含三个写入文件与备份;best-effort,成败不影响主流程)。
func cleanupTmp(ctx context.Context, h Host, p privilege) {
	_, _, _ = execPriv(ctx, h, p, "rm", "-rf", tmpDir)
}

// trimOut 取命令输出的人话摘要(stderr 优先,截断)。
func trimOut(res runResult) string {
	out := strings.TrimSpace(firstNonEmpty(res.stderr, res.stdout))
	if len(out) > 400 {
		out = out[:400] + "…"
	}
	return out
}

// firstNonEmpty 返回首个非空白字符串。
func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
