// upload.go — .vma 备份上传(建机恢复源准备)。
//
// 版本自适应(官方 apidoc 验证):PVE 9.x 上传端点 enum 仅 iso|vztmpl|import(无 backup),
// 故 9.x 直接经 SSH 流式写到文件型存储的 dump 目录,恢复用 archive=绝对路径;
// 8.x 存储启用 backup 内容时走 API multipart 上传(POST .../content),返回卷 ID。
// 路由在读取流之前判定(避免流被消费后无法兜底);全程流式,绝不整文件缓冲。
package pveadmin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// UploadBackupTimeout 是单次上传(含 SSH 流式写)的总时限,HTTP 读取期限与其一致。
const UploadBackupTimeout = 2 * time.Hour

// backupFileRe 是 QEMU 备份文件名白名单(.vma 及其压缩形态;LXC tar 恢复尚未支持)。
var backupFileRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,255}\.vma(\.(gz|zst|lzo|bz2|xz))?$`)

// writeBackupAtomic 先写随机临时文件,再以硬链接原子发布;ln 在目标已存在时失败,绝不覆盖备份。
func (s *service) writeBackupAtomic(ctx context.Context, serverID, remote string, content io.Reader) error {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("pveadmin: 创建上传临时文件名: %w", err)
	}
	tmp := remote + ".pipewright-" + hex.EncodeToString(nonce[:]) + ".part"
	cleanup := func() {
		cctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		defer cancel()
		_, _ = s.runner.Run(cctx, serverID, []string{"rm", "-f", "--", tmp})
	}
	if err := s.runner.WriteFile(ctx, serverID, tmp, 0o640, content); err != nil {
		cleanup()
		return err
	}
	if _, err := s.runner.Run(ctx, serverID, []string{"ln", "--", tmp, remote}); err != nil {
		cleanup()
		return fmt.Errorf("%w: 无法发布备份(目标可能已存在): %v", ErrInvalidInput, err)
	}
	cleanup()
	return nil
}

// UploadBackup 把 .vma 备份流式落到节点存储,返回恢复源(存储卷 ID 或绝对路径)。
// 写操作:readonly 闸门。content 只读一次(路由前置判定,无消费后兜底)。
func (s *service) UploadBackup(ctx context.Context, id, node, storage, fileName string, content io.Reader) (string, error) {
	if !nameRe.MatchString(node) || !nameRe.MatchString(storage) {
		return "", fmt.Errorf("%w: 节点名/存储名不合规", ErrInvalidInput)
	}
	if !backupFileRe.MatchString(fileName) {
		return "", fmt.Errorf("%w: 备份文件名须为 .vma(可带 gz/zst/lzo/bz2/xz 压缩后缀)", ErrInvalidInput)
	}
	if content == nil {
		return "", fmt.Errorf("%w: 上传内容为空", ErrInvalidInput)
	}
	cl, c, err := s.clientFor(ctx, id)
	if err != nil {
		return "", err
	}
	if cl.Readonly {
		return "", fmt.Errorf("%w: 集群 %s 处于只读模式,请在设置页解锁后再上传备份", ErrReadonly, cl.Name)
	}
	ctx, cancel := context.WithTimeout(ctx, UploadBackupTimeout)
	defer cancel()

	major := apiMajor(ctx, c)
	entries, err := storagesFor(ctx, c, node)
	if err != nil {
		return "", err
	}
	var entry *apiStorageEntry
	for i := range entries {
		if entries[i].Storage == storage {
			entry = &entries[i]
			break
		}
	}
	if entry == nil {
		return "", fmt.Errorf("%w: 节点 %s 无存储 %s", ErrInvalidInput, node, storage)
	}

	// 9.x API 不收 backup;8.x 存储未启用 backup 内容 likewise → SSH 文件路径。
	if major >= 9 || !contentHas(entry.Content, "backup") {
		cfg, cerr := storageConfig(ctx, c, storage)
		if cerr != nil {
			return "", cerr
		}
		path, _ := cfg["path"].(string)
		if path == "" {
			return "", fmt.Errorf("%w: 存储 %s 非文件型(取不到路径),无法上传 .vma;PVE 9.x API 不支持 backup 上传,请改用文件型存储(dir/nfs)", ErrInvalidInput, storage)
		}
		serverID, serr := s.nodeServerID(ctx, cl, node)
		if serr != nil {
			return "", serr
		}
		remote := strings.TrimRight(path, "/") + "/dump/" + fileName
		if werr := s.writeBackupAtomic(ctx, serverID, remote, content); werr != nil {
			return "", werr
		}
		return remote, nil
	}

	// 8.x API multipart 上传(content=backup)。
	var volid string
	path := "/api2/json/nodes/" + node + "/storage/" + storage + "/content"
	if err := c.Upload(ctx, path, url.Values{"content": {"backup"}, "filename": {fileName}}, "filename", fileName, content, &volid); err != nil {
		return "", err
	}
	if volid == "" {
		return "", fmt.Errorf("%w: 上传响应缺卷 ID", ErrConnectFailed)
	}
	return volid, nil
}
