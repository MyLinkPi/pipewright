// backups.go:实例备份与恢复。
//
// 备份(引擎原生 dump)走「远端临时文件 + 流式回拉」:先在目标机把 dump 落到
// /tmp(退出码/stderr 可见,失败即知),再经 target.Download(cat)流式写回控制机
// <DataDir>/dbaas-backups/<id>/,最后清远端临时文件。全程无全量内存驻留。
// 恢复为反向:Upload 到远端临时文件 → docker exec -i 流式回放 → 清理。
//
// 口令出现在目标机执行的命令行里(docker exec -e / sh -c 内联)——目标机 compose
// 本就明文持有口令,不新增暴露面;但**错误信息绝不携带 argv**(防口令进日志/审计)。
package dbaas

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/huangchengsir/pipewright/internal/composeops"
)

// Backup 是一次备份的记录视图。
type Backup struct {
	ID         string
	InstanceID string
	FileName   string
	SizeBytes  int64
	Sha256     string
	Kind       string // manual|scheduled|pre_restore
	Status     string // running|completed|failed
	Error      string
	ObjectKey  string
	CreatedAt  string
	FinishedAt string
}

// primaryContainer 取栈的唯一容器 ID(单服务栈;取首个十六进制 ID)。
func (s *Service) primaryContainer(ctx context.Context, inst *Instance) (string, error) {
	ids, err := composeops.ProjectContainerIDs(ctx, s.targets, inst.ServerID, inst.StackName)
	if err != nil {
		return "", fmt.Errorf("取容器失败: %w", err)
	}
	if len(ids) == 0 {
		return "", fmt.Errorf("栈 %s 无运行容器(实例可能已停止)", inst.StackName)
	}
	return ids[0], nil
}

// RunBackup 执行一次备份(同步;调用方可包 goroutine)。kind: manual/scheduled/pre_restore。
func (s *Service) RunBackup(ctx context.Context, instanceID, kind string) (*Backup, error) {
	inst, err := s.Get(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.inflight[inst.ID] {
		s.mu.Unlock()
		return nil, ErrBusy
	}
	s.inflight[inst.ID] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.inflight, inst.ID)
		s.mu.Unlock()
	}()

	rec := &Backup{
		ID: "dbb-" + uuid.NewString(), InstanceID: inst.ID,
		Kind: kind, Status: "running", CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	ext := "sql"
	if inst.Engine == "redis" {
		ext = "rdb"
	}
	rec.FileName = fmt.Sprintf("%s-%s.%s", inst.Name, strings.ReplaceAll(time.Now().Format("20060102-150405"), ".", ""), ext)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO dbaas_backups (id, instance_id, file_name, size_bytes, sha256, kind, status, error, object_key, created_at) VALUES (?, ?, ?, 0, '', ?, 'running', '', '', ?)`,
		rec.ID, rec.InstanceID, rec.FileName, rec.Kind, rec.CreatedAt); err != nil {
		return nil, fmt.Errorf("dbaas: 备份登记失败: %w", err)
	}

	password, err := s.RevealPassword(ctx, inst.ID)
	if err != nil {
		s.failBackup(rec, err)
		return rec, err
	}
	dir := filepath.Join(s.BackupsRoot(), inst.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.failBackup(rec, err)
		return rec, err
	}
	localPath := filepath.Join(dir, rec.FileName)

	size, sum, err := s.dumpRemoteToLocal(ctx, inst, password, localPath)
	finished := time.Now().UTC().Format(time.RFC3339)
	if err != nil {
		_ = os.Remove(localPath)
		rec.Status, rec.Error, rec.FinishedAt = "failed", err.Error(), finished
		s.finishBackup(rec)
		s.logf("备份失败(%s):%v", inst.Name, err)
		return rec, err
	}
	rec.Status, rec.SizeBytes, rec.Sha256, rec.FinishedAt = "completed", size, sum, finished
	s.finishBackup(rec)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE dbaas_instances SET last_backup_at = ? WHERE id = ?`, rec.CreatedAt, inst.ID); err != nil {
		s.logf("更新 last_backup_at 失败(best-effort):%v", err)
	}
	s.logf("备份完成:%s → %s(%d 字节)", inst.Name, rec.FileName, size)
	s.applyRetention(ctx, inst)
	return rec, nil
}

func (s *Service) failBackup(rec *Backup, err error) {
	rec.Status, rec.Error = "failed", err.Error()
	rec.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	s.finishBackup(rec)
}

func (s *Service) finishBackup(rec *Backup) {
	if _, err := s.db.Exec(
		`UPDATE dbaas_backups SET size_bytes = ?, sha256 = ?, status = ?, error = ?, object_key = ?, finished_at = ? WHERE id = ?`,
		rec.SizeBytes, rec.Sha256, rec.Status, rec.Error, rec.ObjectKey, rec.FinishedAt, rec.ID); err != nil {
		s.logf("更新备份记录失败(%s):%v", rec.ID, err)
	}
}

// dumpRemoteToLocal 在目标机执行引擎 dump 到 /tmp 临时文件,流式回拉并清理。
// 返回 (大小, sha256)。错误信息绝不包含执行的命令(口令在 argv 里)。
func (s *Service) dumpRemoteToLocal(ctx context.Context, inst *Instance, password, localPath string) (int64, string, error) {
	cid, err := s.primaryContainer(ctx, inst)
	if err != nil {
		return 0, "", err
	}
	remote := fmt.Sprintf("/tmp/pw-dbaas-%s.%s", uuid.NewString(), filepath.Ext(recFileName(localPath)))
	var script string
	switch inst.Engine {
	case "mysql":
		script = fmt.Sprintf(`docker exec -e MYSQL_PWD=%s %s mysqldump -uroot --all-databases --single-transaction --routines --triggers --events > %s`,
			shq(password), cid, remote)
	case "postgres":
		script = fmt.Sprintf(`docker exec -e PGPASSWORD=%s %s pg_dumpall -U postgres > %s`,
			shq(password), cid, remote)
	case "redis":
		script = fmt.Sprintf(`docker exec %s sh -c 'redis-cli --no-auth-warning -a %s --rdb /dev/stdout' > %s`,
			cid, shSingle(password), remote)
	default:
		return 0, "", fmt.Errorf("%w: %s", ErrUnsupported, inst.Engine)
	}
	if res, err := s.targets.Exec(ctx, inst.ServerID, []string{"sh", "-c", script}); err != nil {
		return 0, "", fmt.Errorf("远端 dump 执行失败: %v", err)
	} else if res.ExitCode != 0 {
		return 0, "", fmt.Errorf("远端 dump 失败: %s", trimOut(res.Stderr, 512))
	}
	defer func() {
		_, _ = s.targets.Exec(context.Background(), inst.ServerID, []string{"rm", "-f", remote})
	}()

	out, err := os.OpenFile(localPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = out.Close() }()
	h := sha256.New()
	// downloadTo 内部以 cat 流式回拉,读尽后校验远端退出码。
	if err := s.downloadTo(ctx, inst.ServerID, remote, io.MultiWriter(out, h)); err != nil {
		return 0, "", fmt.Errorf("备份回拉失败: %w", err)
	}
	st, err := out.Stat()
	if err != nil {
		return 0, "", err
	}
	if st.Size() == 0 {
		return 0, "", fmt.Errorf("备份数据为空(dump 未产出)")
	}
	return st.Size(), hex.EncodeToString(h.Sum(nil)), nil
}

// recFileName 仅为可读性保留(本地路径 → 文件名)。
func recFileName(p string) string { return filepath.Base(p) }

// RestoreInput 是恢复来源:BackupID(既有备份)或 UploadPath(HTTP 层暂存的本地文件)。
type RestoreInput struct {
	BackupID   string
	UploadPath string // 控制机本地 .sql/.rdb 文件(用后由调用方清理)
}

// Restore 恢复实例:先自动做一份 pre_restore 安全备份,再执行恢复。返回安全备份记录。
func (s *Service) Restore(ctx context.Context, instanceID string, in RestoreInput) (*Backup, error) {
	inst, err := s.Get(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	var localPath string
	switch {
	case in.BackupID != "":
		bk, err := s.GetBackup(ctx, in.BackupID)
		if err != nil {
			return nil, err
		}
		if bk.InstanceID != inst.ID {
			return nil, fmt.Errorf("%w: 备份不属于该实例", ErrInvalidInput)
		}
		if bk.Status != "completed" {
			return nil, fmt.Errorf("%w: 仅完成的备份可恢复", ErrInvalidInput)
		}
		localPath = filepath.Join(s.BackupsRoot(), inst.ID, bk.FileName)
	default:
		localPath = in.UploadPath
	}
	if localPath == "" {
		return nil, fmt.Errorf("%w: 缺少恢复来源", ErrInvalidInput)
	}

	// 恢复前强制安全备份(恢复失败时兜底)。
	safety, _ := s.RunBackup(ctx, inst.ID, "pre_restore")
	if safety != nil && safety.Status != "completed" {
		return safety, fmt.Errorf("恢复前安全备份失败,已中止恢复: %s", safety.Error)
	}

	password, err := s.RevealPassword(ctx, inst.ID)
	if err != nil {
		return safety, err
	}
	cid, err := s.primaryContainer(ctx, inst)
	if err != nil {
		return safety, err
	}

	if inst.Engine == "redis" {
		err = s.restoreRedis(ctx, inst, cid, localPath)
	} else {
		err = s.restoreSQL(ctx, inst, cid, password, localPath)
	}
	if err != nil {
		return safety, fmt.Errorf("恢复执行失败: %w", err)
	}
	s.logf("恢复完成:%s ← %s", inst.Name, filepath.Base(localPath))
	return safety, nil
}

// restoreSQL 把 .sql 流式回放进容器(mysql: mysql;postgres: psql)。
func (s *Service) restoreSQL(ctx context.Context, inst *Instance, cid, password, localPath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	remote := fmt.Sprintf("/tmp/pw-dbaas-restore-%s.sql", uuid.NewString())
	if err := s.uploadFrom(ctx, inst.ServerID, f, remote); err != nil {
		return fmt.Errorf("上传恢复文件失败: %w", err)
	}
	defer func() {
		_, _ = s.targets.Exec(context.Background(), inst.ServerID, []string{"rm", "-f", remote})
	}()
	var script string
	if inst.Engine == "mysql" {
		script = fmt.Sprintf(`docker exec -i -e MYSQL_PWD=%s %s mysql -uroot < %s`, shq(password), cid, remote)
	} else {
		script = fmt.Sprintf(`docker exec -i %s psql -U postgres -v ON_ERROR_STOP=1 < %s`, cid, remote)
	}
	res, err := s.targets.Exec(ctx, inst.ServerID, []string{"sh", "-c", script})
	if err != nil {
		return fmt.Errorf("回放失败: %v", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("回放失败: %s", trimOut(res.Stderr, 1024))
	}
	return nil
}

// restoreRedis 用 RDB 替换法:停容器 → 清 AOF 目录(RDB 才会被加载)→ 写入 dump.rdb →
// 启容器(启动后 Redis 以 RDB 重建 AOF)。
func (s *Service) restoreRedis(ctx context.Context, inst *Instance, cid, localPath string) error {
	if _, err := s.targets.Exec(ctx, inst.ServerID, []string{"docker", "stop", cid}); err != nil {
		return fmt.Errorf("停止容器失败: %v", err)
	}
	vols, err := composeops.ProjectVolumeNames(ctx, s.targets, inst.ServerID, inst.StackName)
	if err != nil || len(vols) == 0 {
		return fmt.Errorf("定位数据卷失败")
	}
	// 用已存在的 redis 镜像起一次性容器清 AOF + 挂卷写 RDB(避免额外拉镜像)。
	clear := fmt.Sprintf(`docker run --rm -v %s:/data %s sh -c 'rm -rf /data/appendonlydir /data/dump.rdb'`, vols[0], redisImageFor(inst.Version))
	if res, err := s.targets.Exec(ctx, inst.ServerID, []string{"sh", "-c", clear}); err != nil {
		return fmt.Errorf("清理数据目录失败: %v", err)
	} else if res.ExitCode != 0 {
		return fmt.Errorf("清理数据目录失败: %s", trimOut(res.Stderr, 512))
	}
	write := fmt.Sprintf(`docker run --rm -i -v %s:/data --entrypoint sh %s -c 'cat > /data/dump.rdb'`, vols[0], redisImageFor(inst.Version))
	if err := s.pipeFileToRemote(ctx, inst.ServerID, localPath, write); err != nil {
		return fmt.Errorf("写入 RDB 失败: %w", err)
	}
	if _, err := s.targets.Exec(ctx, inst.ServerID, []string{"docker", "start", cid}); err != nil {
		return fmt.Errorf("重启容器失败: %v", err)
	}
	return nil
}

// redisImageFor 返回 redis 镜像引用(与部署模板一致)。
func redisImageFor(version string) string { return "redis:" + version + "-alpine" }

// ListBackups 列实例的备份(新→老)。
func (s *Service) ListBackups(ctx context.Context, instanceID string) ([]Backup, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, instance_id, file_name, size_bytes, sha256, kind, status, error, object_key, created_at, finished_at
		 FROM dbaas_backups WHERE instance_id = ? ORDER BY created_at DESC, id DESC`, instanceID)
	if err != nil {
		return nil, fmt.Errorf("dbaas: list backups: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Backup
	for rows.Next() {
		var b Backup
		if err := rows.Scan(&b.ID, &b.InstanceID, &b.FileName, &b.SizeBytes, &b.Sha256, &b.Kind, &b.Status, &b.Error, &b.ObjectKey, &b.CreatedAt, &b.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// GetBackup 取单条备份记录。
func (s *Service) GetBackup(ctx context.Context, id string) (*Backup, error) {
	var b Backup
	err := s.db.QueryRowContext(ctx,
		`SELECT id, instance_id, file_name, size_bytes, sha256, kind, status, error, object_key, created_at, finished_at
		 FROM dbaas_backups WHERE id = ?`, id,
	).Scan(&b.ID, &b.InstanceID, &b.FileName, &b.SizeBytes, &b.Sha256, &b.Kind, &b.Status, &b.Error, &b.ObjectKey, &b.CreatedAt, &b.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// BackupPath 返回备份文件磁盘路径。
func (s *Service) BackupPath(b *Backup) string {
	return filepath.Join(s.BackupsRoot(), b.InstanceID, b.FileName)
}

// DeleteBackup 删除备份记录与磁盘文件(幂等)。
func (s *Service) DeleteBackup(ctx context.Context, id string) error {
	b, err := s.GetBackup(ctx, id)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM dbaas_backups WHERE id = ?`, id); err != nil {
		return err
	}
	_ = os.Remove(s.BackupPath(b))
	return nil
}

// UploadBackupToObjectStore 把备份上传到对象存储(键 dbaas/<实例名>/<文件>)。
func (s *Service) UploadBackupToObjectStore(ctx context.Context, id, providerID string) error {
	if s.opt.Uploader == nil {
		return fmt.Errorf("dbaas: 对象存储服务未装配")
	}
	b, err := s.GetBackup(ctx, id)
	if err != nil {
		return err
	}
	if b.Status != "completed" {
		return fmt.Errorf("dbaas: 只有完成的备份才能上传")
	}
	f, err := os.Open(s.BackupPath(b))
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	var instName string
	_ = s.db.QueryRowContext(ctx, `SELECT name FROM dbaas_instances WHERE id = ?`, b.InstanceID).Scan(&instName)
	if instName == "" {
		instName = b.InstanceID
	}
	key := "dbaas/" + instName + "/" + b.FileName
	if err := s.opt.Uploader.Upload(ctx, providerID, key, f); err != nil {
		return err
	}
	b.ObjectKey = key
	s.finishBackup(b)
	return nil
}

// applyRetention 按实例保留份数裁剪最老备份(连同磁盘文件)。
func (s *Service) applyRetention(ctx context.Context, inst *Instance) {
	if inst.BackupRetention <= 0 {
		return
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, file_name FROM dbaas_backups WHERE instance_id = ? AND status = 'completed' ORDER BY created_at DESC, id DESC`, inst.ID)
	if err != nil {
		return
	}
	type idFile struct{ id, name string }
	var all []idFile
	for rows.Next() {
		var e idFile
		if rows.Scan(&e.id, &e.name) == nil {
			all = append(all, e)
		}
	}
	_ = rows.Close()
	if len(all) <= inst.BackupRetention {
		return
	}
	for _, e := range all[inst.BackupRetention:] {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM dbaas_backups WHERE id = ?`, e.id); err == nil && e.name != "" {
			_ = os.Remove(filepath.Join(s.BackupsRoot(), inst.ID, e.name))
		}
	}
}

// trimOut 截断输出用于错误信息(只含 stderr,不含命令)。
func trimOut(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) > max {
		return s[:max] + "…"
	}
	if s == "" {
		return "(无输出)"
	}
	return s
}

// shq 把口令安全地放进 sh 双引号上下文(字符集已排除 " 与 \,这里仍做防御转义)。
func shq(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`")
	return `"` + r.Replace(s) + `"`
}

// shSingle 把口令安全地放进 sh 单引号上下文。
func shSingle(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
