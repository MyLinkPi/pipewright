// restore.go:恢复编排。来源三种:本实例已有备份(backupId)、控制机暂存文件
// (HTTP 上传/云端直拉由 httpapi 落盘后传路径)、恢复前一律强制 pre_restore 安全备份。
// 只读实例拒绝恢复(与 dbadmin.Import 同纪律);PG 的 pg_dumpall 产物(含 \connect
// 元命令)走 SSH 容器内 psql,协议子集产物按 `-- PWDB:` 段逐库连接回放。
package dbbackup

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/huangchengsir/pipewright/internal/backup"
)

// RestoreInput 是恢复来源:BackupID(既有备份)或 LocalPath(控制机暂存文件,用后由
// 调用方清理)。
type RestoreInput struct {
	BackupID  string
	LocalPath string
}

// Restore 恢复实例,返回自动创建的安全备份记录(pre_restore;恢复失败时兜底)。
// 同步执行且与请求 ctx 解耦(半途取消比跑完更危险);与备份互斥(acquire)。
func (s *Service) Restore(ctx context.Context, kind, instanceID string, in RestoreInput) (*Record, error) {
	ref, err := s.ref(ctx, kind, instanceID)
	if err != nil {
		return nil, err
	}
	capability, err := s.Capability(ctx, kind, instanceID)
	if err != nil {
		return nil, err
	}
	if !capability.Restoreable {
		return nil, fmt.Errorf("%w:%s", ErrUnsupported, capability.Note)
	}
	if ref.ReadOnly {
		return nil, ErrReadonly
	}

	var localPath string
	switch {
	case in.BackupID != "":
		rec, err := s.Get(ctx, in.BackupID)
		if err != nil {
			return nil, err
		}
		if rec.InstanceID != ref.ID {
			return nil, fmt.Errorf("%w: 备份不属于该实例", ErrInvalidInput)
		}
		if rec.Status != "completed" {
			return nil, fmt.Errorf("%w: 仅完成的备份可恢复", ErrInvalidInput)
		}
		localPath = s.BackupPath(rec)
	default:
		localPath = in.LocalPath
	}
	if strings.TrimSpace(localPath) == "" {
		return nil, fmt.Errorf("%w: 缺少恢复来源", ErrInvalidInput)
	}

	// 恢复前强制安全备份(失败即中止恢复)。
	safety, err := s.RunBackup(ctx, kind, instanceID, "pre_restore")
	if err != nil {
		return nil, fmt.Errorf("恢复前安全备份失败,已中止恢复: %w", err)
	}
	safetyRec, serr := s.waitRecord(safety.ID)
	if serr != nil || safetyRec.Status != "completed" {
		status, errMsg := serr.Error(), ""
		if safetyRec != nil {
			status, errMsg = safetyRec.Status, safetyRec.Error
		}
		return safety, fmt.Errorf("恢复前安全备份失败(%s %s),已中止恢复", status, errMsg)
	}

	release, ok := s.acquire(kind, instanceID)
	if !ok {
		return safety, ErrBusy
	}
	defer release()

	// 与请求生命周期解耦:页面关闭不中断回放。
	detached := context.Background()
	switch {
	case ref.Kind == KindMySQL:
		err = s.restoreMySQL(detached, ref, localPath)
	case ref.Kind == KindPostgres:
		if pgNeedsPsql(localPath) {
			if !ref.managed() {
				err = fmt.Errorf("%w: 该备份为 pg_dumpall 全量格式,只能在纳管主机实例上回放", ErrUnsupported)
			} else {
				err = s.restorePostgresSSH(detached, ref, localPath)
			}
		} else {
			err = s.restorePostgresProtocol(detached, ref, localPath)
		}
	case ref.Kind == KindRedis:
		if !ref.managed() {
			err = fmt.Errorf("%w: 外部 Redis 不支持恢复", ErrUnsupported)
		} else {
			err = s.restoreRedisSSH(detached, ref, localPath)
		}
	}
	if err != nil {
		return safety, fmt.Errorf("恢复执行失败: %w", err)
	}
	s.logf("恢复完成:%s ← %s", ref.Name, filepathBase(localPath))
	return safety, nil
}

func filepathBase(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	if i := strings.LastIndexByte(p, '\\'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// waitRecord 轮询等备份终态(恢复前安全备份是异步跑的;上限 30 分钟)。
func (s *Service) waitRecord(id string) (*Record, error) {
	deadline := 1800
	for i := 0; i < deadline; i++ {
		rec, err := s.Get(context.Background(), id)
		if err != nil {
			return nil, err
		}
		if rec.Status == "completed" || rec.Status == "failed" {
			return rec, nil
		}
		sleepSec(1)
	}
	return nil, errors.New("等待安全备份超时")
}

// pgNeedsPsql 判断 dump 是否为 pg_dumpall 产物(含 \connect 元命令,只能 psql 回放)。
func pgNeedsPsql(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "\\connect") {
			return true
		}
	}
	return false
}

// restoreMySQL 全库回放(服务器级连接,分块 multiStatements)。
func (s *Service) restoreMySQL(ctx context.Context, ref *instanceRef, dumpPath string) error {
	db, err := s.dba.ConnectBackup(ctx, ref.ID, "")
	if err != nil {
		return fmt.Errorf("连接实例失败: %w", err)
	}
	defer func() { _ = db.Close() }()
	f, err := os.Open(dumpPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return backup.ReplayChunkedSQL(db, f)
}

// restorePostgresProtocol 协议子集产物回放:按 `-- PWDB:` 段逐库连接回放。
func (s *Service) restorePostgresProtocol(ctx context.Context, ref *instanceRef, dumpPath string) error {
	f, err := os.Open(dumpPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 64<<20)

	var cur strings.Builder
	curDB := ""
	replay := func(dbname string, section *strings.Builder) error {
		if dbname == "" || section.Len() == 0 {
			return nil
		}
		db, err := s.dba.ConnectBackup(ctx, ref.ID, dbname)
		if err != nil {
			return fmt.Errorf("连接数据库 %s 失败: %w", dbname, err)
		}
		defer func() { _ = db.Close() }()
		return backup.ReplayChunkedSQL(db, strings.NewReader(section.String()))
	}
	for scanner.Scan() {
		line := scanner.Text()
		if db := pwdbName(line); db != "" && db != curDB {
			if err := replay(curDB, &cur); err != nil {
				return err
			}
			cur.Reset()
			curDB = db
			continue
		}
		cur.WriteString(line)
		cur.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("读恢复文件失败: %w", err)
	}
	return replay(curDB, &cur)
}

// pwdbName 匹配 `-- PWDB: <name>` 段标记行,返回库名(非标记行返回空)。
func pwdbName(line string) string {
	const prefix = "-- PWDB: "
	if strings.HasPrefix(line, prefix) {
		return strings.TrimSpace(strings.TrimPrefix(line, prefix))
	}
	return ""
}
