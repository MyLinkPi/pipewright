// ssh_ops.go:SSH+docker exec 备份路径(纳管主机上的实例——应用商店部署)。
// 全部经 target.Service 的 array 化命令执行;密码出现在目标机执行的 argv/env 里
// (目标机容器本就持有该密码,不新增暴露面),但**错误信息绝不携带 argv**(防进日志/审计)。
package dbbackup

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/google/uuid"
)

// shq 把口令安全地放进 sh 双引号上下文(字符集受限,仍做防御转义)。
func shq(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`")
	return `"` + r.Replace(s) + `"`
}

// dumpPostgresSSH 容器内 pg_dumpall 全保真导出(远端 /tmp 中转 → 流式回拉)。
func (s *Service) dumpPostgresSSH(ctx context.Context, ref *instanceRef, w io.Writer) error {
	cid, err := s.containerID(ctx, ref)
	if err != nil {
		return err
	}
	password, err := s.dba.RevealPassword(ctx, ref.ID)
	if err != nil {
		return fmt.Errorf("取实例凭据失败: %w", err)
	}
	remote := "/tmp/pw-dbbackup-" + uuid.NewString() + ".sql"
	script := fmt.Sprintf(`docker exec -e PGPASSWORD=%s %s pg_dumpall -U postgres > %s`, shq(password), cid, remote)
	defer func() {
		_, _ = s.targets.Exec(context.Background(), ref.ServerID, []string{"rm", "-f", remote})
	}()
	if res, err := s.targets.Exec(ctx, ref.ServerID, []string{"sh", "-c", script}); err != nil {
		return fmt.Errorf("远端导出执行失败: %v", err)
	} else if res.ExitCode != 0 {
		return fmt.Errorf("远端导出失败: %s", trimOut(res.Stderr, 512))
	}
	if _, err := s.downloadTo(ctx, ref.ServerID, remote, w); err != nil {
		return fmt.Errorf("备份回拉失败: %w", err)
	}
	return nil
}

// dumpRedisSSH 容器内 redis-cli 导出 RDB(--rdb /dev/stdout 流式;远端 /tmp 中转回拉)。
// 认证走 REDISCLI_AUTH 环境变量(redis-cli 官方支持)——口令不进远端 ps 的 argv。
func (s *Service) dumpRedisSSH(ctx context.Context, ref *instanceRef, w io.Writer) error {
	cid, err := s.containerID(ctx, ref)
	if err != nil {
		return err
	}
	password, err := s.redisa.RevealPassword(ctx, ref.ID)
	if err != nil {
		return fmt.Errorf("取实例凭据失败: %w", err)
	}
	remote := "/tmp/pw-dbbackup-" + uuid.NewString() + ".rdb"
	envPart := ""
	if password != "" {
		envPart = " -e REDISCLI_AUTH=" + shq(password)
	}
	script := fmt.Sprintf(`docker exec%s %s redis-cli --no-auth-warning --rdb /dev/stdout > %s`,
		envPart, cid, remote)
	defer func() {
		_, _ = s.targets.Exec(context.Background(), ref.ServerID, []string{"rm", "-f", remote})
	}()
	if res, err := s.targets.Exec(ctx, ref.ServerID, []string{"sh", "-c", script}); err != nil {
		return fmt.Errorf("远端导出执行失败: %v", err)
	} else if res.ExitCode != 0 {
		return fmt.Errorf("远端导出失败: %s", trimOut(res.Stderr, 512))
	}
	if _, err := s.downloadTo(ctx, ref.ServerID, remote, w); err != nil {
		return fmt.Errorf("备份回拉失败: %w", err)
	}
	return nil
}

// restorePostgresSSH pg_dumpall 产物(含 \connect 元命令)只能经容器内 psql 回放:
// 上传到远端 /tmp → docker exec -i psql < 文件 → 清理。
func (s *Service) restorePostgresSSH(ctx context.Context, ref *instanceRef, dumpPath string) error {
	cid, err := s.containerID(ctx, ref)
	if err != nil {
		return err
	}
	remote := "/tmp/pw-dbrestore-" + uuid.NewString() + ".sql"
	f, err := os.Open(dumpPath)
	if err != nil {
		return err
	}
	upErr := s.targets.Upload(ctx, ref.ServerID, f, remote)
	_ = f.Close()
	if upErr != nil {
		return fmt.Errorf("上传恢复文件失败: %w", upErr)
	}
	defer func() {
		_, _ = s.targets.Exec(context.Background(), ref.ServerID, []string{"rm", "-f", remote})
	}()
	script := fmt.Sprintf(`docker exec -i %s psql -U postgres -v ON_ERROR_STOP=1 -f %s`, cid, remote)
	if res, err := s.targets.Exec(ctx, ref.ServerID, []string{"sh", "-c", script}); err != nil {
		return fmt.Errorf("回放执行失败: %v", err)
	} else if res.ExitCode != 0 {
		return fmt.Errorf("回放失败: %s", trimOut(res.Stderr, 1024))
	}
	return nil
}

// restoreRedisSSH RDB 替换法:停容器 → 定位 /data 卷 → 用栈同镜像的一次性容器清
// AOF 目录与旧 dump → stdin 流式写入新 dump.rdb → 起容器(启动后以 RDB 重建 AOF)。
func (s *Service) restoreRedisSSH(ctx context.Context, ref *instanceRef, dumpPath string) error {
	cid, err := s.containerID(ctx, ref)
	if err != nil {
		return err
	}
	// 镜像取栈容器自身(避免硬编码版本,也避免额外拉镜像)。
	res, err := s.targets.Exec(ctx, ref.ServerID,
		[]string{"docker", "inspect", "--format", "{{.Config.Image}}", cid})
	if err != nil || res.ExitCode != 0 {
		return fmt.Errorf("取容器镜像失败")
	}
	image := strings.TrimSpace(res.Stdout)
	if image == "" || strings.ContainsAny(image, " \t'\";$`\\") {
		return fmt.Errorf("容器镜像名异常,拒绝执行")
	}
	// 数据卷名:从容器 Mounts 里找目的为 /data 的具名卷。
	res, err = s.targets.Exec(ctx, ref.ServerID,
		[]string{"docker", "inspect", "--format",
			`{{range .Mounts}}{{if eq .Destination "/data"}}{{.Name}}{{end}}{{end}}`, cid})
	if err != nil || res.ExitCode != 0 {
		return fmt.Errorf("定位数据卷失败")
	}
	vol := strings.TrimSpace(res.Stdout)
	if vol == "" || strings.ContainsAny(vol, " \t'\";$`\\") {
		return fmt.Errorf("定位数据卷失败(容器无 /data 具名卷)")
	}

	if rres, rerr := s.targets.Exec(ctx, ref.ServerID, []string{"docker", "stop", cid}); rerr != nil || rres.ExitCode != 0 {
		return fmt.Errorf("停止容器失败")
	}
	clearScript := fmt.Sprintf(`docker run --rm -v %s:/data %s sh -c 'rm -rf /data/appendonlydir /data/dump.rdb'`, vol, image)
	if rres, rerr := s.targets.Exec(ctx, ref.ServerID, []string{"sh", "-c", clearScript}); rerr != nil {
		return fmt.Errorf("清理数据目录失败: %v", rerr)
	} else if rres.ExitCode != 0 {
		return fmt.Errorf("清理数据目录失败: %s", trimOut(rres.Stderr, 512))
	}
	if err := s.pipeFileToRemote(ctx, ref.ServerID, dumpPath,
		fmt.Sprintf(`docker run --rm -i -v %s:/data --entrypoint sh %s -c 'cat > /data/dump.rdb'`, vol, image)); err != nil {
		return fmt.Errorf("写入 RDB 失败: %w", err)
	}
	if rres, rerr := s.targets.Exec(ctx, ref.ServerID, []string{"docker", "start", cid}); rerr != nil || rres.ExitCode != 0 {
		return fmt.Errorf("重启容器失败")
	}
	return nil
}

// downloadTo / pipeFileToRemote 的薄封装(见 io.go)。
func (s *Service) downloadTo(ctx context.Context, serverID, remotePath string, w io.Writer) (int64, error) {
	return s.targets.Download(ctx, serverID, remotePath, w)
}
