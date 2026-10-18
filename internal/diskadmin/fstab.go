// fstab.go — /etc/fstab 持久化(备份-校验-原子替换,对齐 registryhub/daemon.go 的
// 配置变更范式)。
//
// 写入纪律:
//   - 新内容先落到临时文件(内容经 stdin 喂给 `cat > "$0"`,同 target.Upload 纪律,
//     绝不进 argv);
//   - findmnt 可用时对临时文件做 --verify 校验:原 fstab 本就校验通过而新文件不过
//     → 我们的改动引入问题,清理临时文件、绝不动 /etc/fstab;原 fstab 本就不过
//     (存量脏条目)则放行(已留备份);
//   - 提交 = cp 备份 + mv 原子替换(同文件系统);
//   - 条目一律以 UUID= 为源(采不到 UUID 才退回设备路径),杜绝设备名漂移。
package diskadmin

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	fstabPath    = "/etc/fstab"
	fstabTmpPath = "/etc/.fstab.pw-tmp"
)

// fstabAdd 追加一条挂载条目(该挂载点已有条目则不动;幂等)。
func (s *service) fstabAdd(ctx context.Context, serverID, spec, mnt, fstype, options string, pass int) error {
	cur, err := s.fstabRead(ctx, serverID)
	if err != nil {
		return err
	}
	if fstabHasMountpoint(cur, mnt) {
		return nil
	}
	entry := fmt.Sprintf("%s %s %s %s 0 %d\n", spec, mnt, fstype, options, pass)
	next := cur
	if next != "" && !strings.HasSuffix(next, "\n") {
		next += "\n"
	}
	next += entry
	return s.fstabCommit(ctx, serverID, next)
}

// fstabRemove 移除挂载点对应的条目(无条目则不动;幂等)。
func (s *service) fstabRemove(ctx context.Context, serverID, mnt string) error {
	cur, err := s.fstabRead(ctx, serverID)
	if err != nil {
		return err
	}
	next, removed := fstabWithoutMountpoint(cur, mnt)
	if !removed {
		return nil
	}
	return s.fstabCommit(ctx, serverID, next)
}

// fstabRead 读取当前 fstab(cat;文件恒存在)。
func (s *service) fstabRead(ctx context.Context, serverID string) (string, error) {
	res, err := s.runner.Run(ctx, serverID, []string{"cat", fstabPath})
	if err != nil {
		return "", err
	}
	return res.Stdout, nil
}

// fstabCommit 落盘:临时文件 → 校验 → 备份 → 原子替换。
func (s *service) fstabCommit(ctx context.Context, serverID, content string) error {
	// 1) 写临时文件(内容走 stdin,cat > "$0" 位置参数式)。
	if _, err := s.runner.RunWithStdin(ctx, serverID, []string{"sh", "-c", `cat > "$0"`, fstabTmpPath}, strings.NewReader(content)); err != nil {
		return err
	}
	// 2) 校验(findmnt 缺失则跳过):原通过而新不过 → 我们的问题,绝不提交。
	if res, err := s.runner.Run(ctx, serverID, []string{"command", "-v", "findmnt"}); err == nil && res.ExitCode == 0 {
		_, curErr := s.runner.Run(ctx, serverID, []string{"findmnt", "--verify", "--tab-file", fstabPath})
		_, newErr := s.runner.Run(ctx, serverID, []string{"findmnt", "--verify", "--tab-file", fstabTmpPath})
		if curErr == nil && newErr != nil {
			_, _ = s.runner.Run(ctx, serverID, []string{"rm", "-f", fstabTmpPath}) // 尽力清理
			return fmt.Errorf("%w: findmnt --verify 未通过,已放弃提交(原 /etc/fstab 未动)\n%v", ErrFstabVerify, newErr)
		}
	}
	// 3) 备份 + 原子替换。
	bak := fmt.Sprintf("/etc/fstab.bak-pw-%d", time.Now().Unix())
	if _, err := s.runner.Run(ctx, serverID, []string{"cp", "-a", fstabPath, bak}); err != nil {
		return err
	}
	if _, err := s.runner.Run(ctx, serverID, []string{"mv", "-f", fstabTmpPath, fstabPath}); err != nil {
		return err
	}
	return nil
}

// fstabHasMountpoint 报告 fstab 是否已有指向该挂载点的条目(注释行忽略)。
func fstabHasMountpoint(content, mnt string) bool {
	_, hit := fstabFilter(content, mnt)
	return hit
}

// fstabWithoutMountpoint 返回移除指定挂载点条目后的内容与是否发生移除。
func fstabWithoutMountpoint(content, mnt string) (string, bool) {
	return fstabFilter(content, mnt)
}

// fstabFilter 逐行过滤:非注释行第二字段等于 mnt 的行被剔除。
func fstabFilter(content, mnt string) (string, bool) {
	var kept []string
	removed := false
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			kept = append(kept, line)
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) >= 2 && fields[1] == mnt {
			removed = true
			continue
		}
		kept = append(kept, line)
	}
	if !removed {
		return content, false
	}
	out := strings.Join(kept, "\n")
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return "", true
	}
	return out + "\n", true
}
