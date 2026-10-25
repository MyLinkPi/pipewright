// io.go:DBaaS 的 SSH 传输薄封装(单点收敛,便于统一错误语义)。
package dbaas

import (
	"context"
	"fmt"
	"io"
	"os"
)

// downloadTo 把远端文件流式拉回写入 w(target.Download:cat <path> 流式 + 退出码校验)。
func (s *Service) downloadTo(ctx context.Context, serverID, remotePath string, w io.Writer) error {
	_, err := s.targets.Download(ctx, serverID, remotePath, w)
	return err
}

// uploadFrom 把 reader 内容流式写到远端(target.Upload:cat > <path>)。
func (s *Service) uploadFrom(ctx context.Context, serverID string, content io.Reader, remotePath string) error {
	return s.targets.Upload(ctx, serverID, content, remotePath)
}

// pipeFileToRemote 打开本地文件作为 stdin,在远端执行 sh 脚本(典型:docker run -i
// + cat > 目标卷文件)。数据经 SSH stdin 直灌,无 argv 长度限制。
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
