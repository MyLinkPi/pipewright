// io.go:小工具(睡眠/SSH stdin 管道)。
package dbbackup

import (
	"context"
	"fmt"
	"os"
	"time"
)

// sleepSec 独立小函数(便于测试短路;当前实现真睡)。
func sleepSec(sec int) { time.Sleep(time.Duration(sec) * time.Second) }

// pipeFileToRemote 打开本地文件作为 stdin,在远端执行 sh 脚本(典型:docker run -i
// + cat > 目标卷文件)。数据经 SSH stdin 直灌,无 argv 长度限制;错误不含命令。
func (s *Service) pipeFileToRemote(ctx context.Context, serverID, localPath, script string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	res, err := s.targets.ExecWithStdin(ctx, serverID, []string{"sh", "-c", script}, f)
	if err != nil {
		return fmt.Errorf("远端写入失败: %v", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("远端写入失败: %s", trimOut(res.Stderr, 512))
	}
	return nil
}
