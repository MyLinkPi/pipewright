package platformhttps

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/huangchengsir/pipewright/internal/target"
)

// 临时落盘路径:三个文件收进登录用户私有的 700 目录(防多用户机上 chmod 前的可读窗口;
// 应用完成后整目录清理,备份文件一并删除)。
const (
	tmpDir        = "/tmp/pipewright-platform-https.d"
	tmpConfPath   = tmpDir + "/pipewright-platform.conf"
	tmpCertPath   = tmpDir + "/fullchain.pem"
	tmpKeyPath    = tmpDir + "/privkey.pem"
	backupConfFmt = tmpDir + "/pipewright-platform.conf.bak"
)

// execOK 跑一条 array 命令并报告退出码是否为 0(传输错误原样返回)。
func execOK(ctx context.Context, tg target.Service, serverID string, cmd ...string) (*target.ExecResult, bool, error) {
	res, err := tg.Exec(ctx, serverID, cmd)
	if err != nil {
		return res, false, err
	}
	return res, res.ExitCode == 0, nil
}

// ---------- 探测 ----------

// detectNginx 探测宿主 nginx / 提权能力 / conf.d include 情况(只读,全部 best-effort 除 SSH 错)。
func detectNginx(ctx context.Context, tg target.Service, serverID string) (*NginxDetect, error) {
	d := &NginxDetect{ServerID: serverID}

	// 1) nginx 是否在 PATH(nginx -v 输出在 stderr)。
	res, err := tg.Exec(ctx, serverID, []string{"nginx", "-v"})
	if err != nil {
		return nil, err
	}
	if res.ExitCode == 0 {
		d.Installed = true
		d.Version = parseNginxVersion(firstNonEmpty(res.Stderr, res.Stdout))
	}

	// 2) 提权能力:root 直接可写;非 root 探测免密 sudo。
	uid, ok, err := execOK(ctx, tg, serverID, "id", "-u")
	if err != nil {
		return nil, err
	}
	if ok && strings.TrimSpace(uid.Stdout) == "0" {
		d.IsRoot = true
		d.SudoOk = true
	} else if _, sudoOk, err := execOK(ctx, tg, serverID, "sudo", "-n", "true"); err == nil && sudoOk {
		d.SudoOk = true
	}

	// 3) 平台管理的 vhost 是否已在(曾应用过)。conf.d 通常全局可读,普通 test 即可。
	if _, ok, _ := execOK(ctx, tg, serverID, "test", "-f", managedConfPath); ok {
		d.ManagedConf = true
	}

	// 4) nginx -T 是否 include conf.d(防配置写入后静默失效;best-effort)。
	// 带提权前缀执行:非 root 裸跑 nginx -T 常因读不到 root 600 的证书/私钥而失败,
	// 会误报「conf.d 未加载」;无提权能力时退化为裸跑(与 Apply 前的探测语义一致)。
	var prefix []string
	if !d.IsRoot && d.SudoOk {
		prefix = []string{"sudo", "-n"}
	}
	if dump, ok, _ := execOK(ctx, tg, serverID, append(prefix, "nginx", "-T")...); ok {
		if strings.Contains(dump.Stdout, "/etc/nginx/conf.d") {
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

// applyPlatformConf 在目标机落位平台 HTTPS 配置并热加载:
//
//	探测 nginx + 提权前缀 → 建 700 临时目录并 Upload 三份文件(私钥再收紧 600)→ 安装证书与
//	conf.d vhost(先删陈旧备份再备份既有同名文件)→ nginx -t 校验(失败回滚,活配置不动)→
//	nginx -T 验证 conf.d 真被 include(失败回滚)→ reload(nginx -s reload,失败回退
//	systemctl reload nginx)→ 清理临时目录(含备份)。
func applyPlatformConf(ctx context.Context, tg target.Service, serverID, domain, upstreamHost string,
	upstreamPort int, httpRedirect bool, certPEM, keyPEM string) error {

	// 1) nginx 就绪 + 提权前缀(root 为空前缀;非 root 要求 sudo -n 可用)。
	if res, err := tg.Exec(ctx, serverID, []string{"nginx", "-v"}); err != nil {
		return err
	} else if res.ExitCode != 0 {
		return ErrNoNginx
	}
	prefix, err := privilegePrefix(ctx, tg, serverID)
	if err != nil {
		return err
	}

	conf := renderPlatformConf(domain, upstreamHost, upstreamPort, httpRedirect)
	certDir := certDir(domain)
	backup := backupConfFmt

	// 2) 临时文件落盘:先建登录用户私有的 700 目录再上传(目录 700 兜底,上传与 chmod
	// 之间不存在同机其他用户可读私钥的窗口)。
	for _, cmd := range [][]string{
		{"mkdir", "-p", tmpDir},
		{"chmod", "700", tmpDir},
	} {
		if res, ok, err := execOK(ctx, tg, serverID, cmd...); err != nil {
			return err
		} else if !ok {
			return fmt.Errorf("%w:创建临时目录失败:%s", ErrApply, trimOut(res))
		}
	}
	if err := tg.Upload(ctx, serverID, bytes.NewReader([]byte(conf)), tmpConfPath); err != nil {
		return err
	}
	if err := tg.Upload(ctx, serverID, bytes.NewReader([]byte(certPEM)), tmpCertPath); err != nil {
		return err
	}
	if err := tg.Upload(ctx, serverID, bytes.NewReader([]byte(keyPEM)), tmpKeyPath); err != nil {
		return err
	}
	defer func() { cleanupTmp(ctx, tg, serverID, prefix) }()
	if _, ok, err := execOK(ctx, tg, serverID, "chmod", "600", tmpKeyPath); err != nil || !ok {
		return fmt.Errorf("%w:收紧私钥临时文件权限失败", ErrApply)
	}

	// 3) 安装证书(conf.d 目录一并确保存在,防源码编译版无该目录)。
	for _, cmd := range [][]string{
		{"mkdir", "-p", certDir, "/etc/nginx/conf.d"},
		{"cp", tmpCertPath, certDir + "/fullchain.pem"},
		{"cp", tmpKeyPath, certDir + "/privkey.pem"},
		{"chmod", "600", certDir + "/privkey.pem"},
	} {
		if res, ok, err := execOK(ctx, tg, serverID, append(prefix, cmd...)...); err != nil {
			return err
		} else if !ok {
			return fmt.Errorf("%w:%s", ErrApply, trimOut(res))
		}
	}

	// 4) 备份既有平台 vhost(文件可能不存在,非零退出可容忍)→ 覆盖安装新配置。
	// 先删陈旧备份(上次运行被中断时可能残留),保证 rollbackConf 还原的只会是本次备份。
	_, _, _ = execOK(ctx, tg, serverID, append(prefix, "rm", "-f", backup)...)
	_, _, _ = execOK(ctx, tg, serverID, append(prefix, "cp", managedConfPath, backup)...)
	if res, ok, err := execOK(ctx, tg, serverID, append(prefix, "cp", tmpConfPath, managedConfPath)...); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("%w:%s", ErrApply, trimOut(res))
	}

	// 5) nginx -t(先测后换已就位:校验失败回滚到备份/摘除我们的文件,活配置语义不变)。
	if res, ok, err := execOK(ctx, tg, serverID, append(prefix, "nginx", "-t")...); err != nil {
		return err
	} else if !ok {
		rollbackConf(ctx, tg, serverID, prefix, backup)
		return fmt.Errorf("%w:nginx -t 校验失败:%s", ErrApply, trimOut(res))
	}

	// 6) nginx -T 验证 conf.d 真被 include(防部分手装 nginx 未 include conf.d,配置静默失效)。
	if res, ok, err := execOK(ctx, tg, serverID, append(prefix, "nginx", "-T")...); err != nil {
		return err
	} else if !ok || !strings.Contains(res.Stdout, confMarker) {
		rollbackConf(ctx, tg, serverID, prefix, backup)
		return fmt.Errorf("%w:配置未被 nginx 加载(conf.d 未被 include),请在该机 nginx.conf 的 http{} 内加入 include /etc/nginx/conf.d/*.conf;", ErrApply)
	}

	// 7) 热加载:nginx -s reload → 失败回退 systemctl reload nginx(不自动 start)。
	if res, ok, err := execOK(ctx, tg, serverID, append(prefix, "nginx", "-s", "reload")...); err != nil {
		return err
	} else if !ok {
		if res2, ok2, err2 := execOK(ctx, tg, serverID, append(prefix, "systemctl", "reload", "nginx")...); err2 != nil || !ok2 {
			return fmt.Errorf("%w:配置已就位但热加载失败(nginx 可能未运行):%s",
				ErrApply, firstNonEmpty(trimOut(res), trimOut(res2)))
		}
	}
	return nil
}

// removePlatformConf 移除平台 vhost 与证书目录并热加载;远端无平台配置时为 no-op(幂等)。
func removePlatformConf(ctx context.Context, tg target.Service, serverID, domain string) error {
	// 无平台配置 → 直接成功(不要求提权/不触 nginx)。
	if _, ok, err := execOK(ctx, tg, serverID, "test", "-f", managedConfPath); err != nil {
		return err
	} else if !ok {
		return nil
	}
	prefix, err := privilegePrefix(ctx, tg, serverID)
	if err != nil {
		return err
	}
	defer func() { cleanupTmp(ctx, tg, serverID, prefix) }()

	for _, cmd := range [][]string{
		{"rm", "-f", managedConfPath},
		{"rm", "-rf", certDir(domain)},
		{"rmdir", certsBaseDir}, // 仅当空目录时成功(非零可容忍)
	} {
		if res, ok, err := execOK(ctx, tg, serverID, append(prefix, cmd...)...); err != nil {
			return err
		} else if !ok && cmd[0] != "rmdir" {
			return fmt.Errorf("%w:%s", ErrApply, trimOut(res))
		}
	}
	// 剩余配置校验 + 热加载(摘除平台 vhost 后立即生效)。
	if res, ok, err := execOK(ctx, tg, serverID, append(prefix, "nginx", "-t")...); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("%w:摘除平台配置后 nginx -t 校验失败(该机其余配置自身有误):%s", ErrApply, trimOut(res))
	}
	if res, ok, err := execOK(ctx, tg, serverID, append(prefix, "nginx", "-s", "reload")...); err != nil {
		return err
	} else if !ok {
		if _, ok2, err2 := execOK(ctx, tg, serverID, append(prefix, "systemctl", "reload", "nginx")...); err2 != nil || !ok2 {
			return fmt.Errorf("%w:配置已摘除但热加载失败(nginx 可能未运行):%s", ErrApply, trimOut(res))
		}
	}
	return nil
}

// ---------- 内部 ----------

// privilegePrefix 返回提权前缀:root → nil(直接执行);非 root → ["sudo","-n"](免密 sudo,
// 探测不可用报 ErrNoPrivilege,绝不交互输密)。
func privilegePrefix(ctx context.Context, tg target.Service, serverID string) ([]string, error) {
	res, err := tg.Exec(ctx, serverID, []string{"id", "-u"})
	if err != nil {
		return nil, err
	}
	if res.ExitCode == 0 && strings.TrimSpace(res.Stdout) == "0" {
		return nil, nil
	}
	if _, ok, err := execOK(ctx, tg, serverID, "sudo", "-n", "true"); err != nil {
		return nil, err
	} else if !ok {
		return nil, fmt.Errorf("%w:SSH 用户非 root 且 sudo -n 不可用,请用 root 登录或为该用户配置免密 sudo", ErrNoPrivilege)
	}
	return []string{"sudo", "-n"}, nil
}

// rollbackConf 在校验失败时恢复活配置:有备份 → 还原;无备份(首次应用)→ 摘除我们的文件。
func rollbackConf(ctx context.Context, tg target.Service, serverID string, prefix []string, backup string) {
	if _, ok, _ := execOK(ctx, tg, serverID, append(prefix, "test", "-f", backup)...); ok {
		_, _, _ = execOK(ctx, tg, serverID, append(prefix, "cp", backup, managedConfPath)...)
		return
	}
	_, _, _ = execOK(ctx, tg, serverID, append(prefix, "rm", "-f", managedConfPath)...)
}

// cleanupTmp 清理临时目录(含三个上传文件与备份;best-effort,成败不影响主流程)。
func cleanupTmp(ctx context.Context, tg target.Service, serverID string, prefix []string) {
	_, _, _ = execOK(ctx, tg, serverID, append(prefix, "rm", "-rf", tmpDir)...)
}

// trimOut 取命令输出的人话摘要(stderr 优先,截断)。
func trimOut(res *target.ExecResult) string {
	if res == nil {
		return ""
	}
	out := strings.TrimSpace(firstNonEmpty(res.Stderr, res.Stdout))
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
