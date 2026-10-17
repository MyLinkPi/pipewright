// ops.go — 集群就绪后的管理操作:主机/OSD 纳管与移除、存储池、RBD 块设备、CephFS。
//
// 全部命令经 bootstrap 主机执行(部署时装好 ceph-common);名称类入参先白名单校验
// 再进 argv;移除类操作(drain/purge/rollback)带数据迁移等待,超时返回人读指引
// (操作幂等,稍后重试即可续走)。
package ceph

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/huangchengsir/pipewright/internal/store"
)

// drainWaitTO 是移除主机/OSD 时等待数据迁移的上限(超时返回「稍后重试」指引)。
const drainWaitTO = 15 * time.Minute

// pollInterval 是迁移/纳管等待轮询间隔。
const pollInterval = 15 * time.Second

// ── 集群 ─────────────────────────────────────────────────────────────────────

// ListClusters 返回全部集群(含 bootstrap 主机名)。
func (s *service) ListClusters(ctx context.Context) ([]*Cluster, error) {
	return s.listClusters(ctx)
}

// GetCluster 返回单个集群视图。
func (s *service) GetCluster(ctx context.Context, id string) (*Cluster, error) {
	return s.getCluster(ctx, id)
}

// ── 主机 ─────────────────────────────────────────────────────────────────────

// ListHosts 合并 DB 意图与 orch 实时视图;集群未 ready 时仅返回 DB 意图。
func (s *service) ListHosts(ctx context.Context, id string) ([]HostView, error) {
	rows, err := s.listHostRows(ctx, id)
	if err != nil {
		return nil, err
	}
	serverNames := map[string]string{}
	if list, err := s.servers.List(ctx); err == nil {
		for _, sv := range list {
			serverNames[sv.ID] = sv.Name
		}
	}
	orch := map[string]OrchHost{}
	cl, err := s.getCluster(ctx, id)
	if err != nil {
		return nil, err
	}
	if cl.State == StateReady {
		if res, err := s.run(ctx, cl, []string{"ceph", "orch", "host", "ls", "-f", "json"}); err == nil {
			for _, oh := range parseOrchHosts(res.Stdout) {
				orch[oh.Host] = oh
			}
		}
	}
	out := make([]HostView, 0, len(rows))
	for _, h := range rows {
		v := HostView{HostSpec: h, ServerName: serverNames[h.ServerID]}
		if h.Hostname != "" {
			if oh, ok := orch[h.Hostname]; ok {
				v.InOrch = true
				v.OrchStatus = oh.Status
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// AddHost 向 ready 集群追加主机:orch host add + 等待纳管 + 可选逐盘加 OSD。
func (s *service) AddHost(ctx context.Context, id string, in AddHostInput) error {
	cl, err := s.readyCluster(ctx, id)
	if err != nil {
		return err
	}
	sv, err := s.servers.Get(ctx, in.ServerID)
	if err != nil {
		return fmt.Errorf("%w: 服务器 %s 不可用: %v", ErrInvalidInput, in.ServerID, err)
	}
	for _, l := range in.Labels {
		if !labelRe.MatchString(l) {
			return fmt.Errorf("%w: 非法标签 %q", ErrInvalidInput, l)
		}
	}
	for _, d := range in.Devices {
		if !deviceRe.MatchString(d) {
			return fmt.Errorf("%w: 非法设备路径 %q", ErrInvalidInput, d)
		}
	}

	// 新主机侧也要 cephadm(orch 经 SSH 从 bootstrap 拉起守护进程;预检运行时)。
	if _, err := s.runHost(ctx, in.ServerID, cmdTimeout, []string{"id", "-u"}); err != nil {
		return fmt.Errorf("主机 %s 预检失败: %w", sv.Name, err)
	}
	if _, err := s.runHost(ctx, in.ServerID, cmdTimeout, []string{"docker", "--version"}); err != nil {
		if _, err2 := s.runHost(ctx, in.ServerID, cmdTimeout, []string{"podman", "--version"}); err2 != nil {
			return fmt.Errorf("主机 %s 无 Docker/Podman: %v", sv.Name, err)
		}
	}
	var hostname string
	if res, err := s.runHost(ctx, in.ServerID, cmdTimeout, []string{"hostname"}); err == nil {
		hostname = strings.TrimSpace(res.Stdout)
	}
	if hostname == "" {
		return fmt.Errorf("主机 %s 无法获取 hostname", sv.Name)
	}

	cmd := []string{"ceph", "orch", "host", "add", hostname, sv.Host}
	cmd = append(cmd, in.Labels...)
	if _, err := s.run(ctx, cl, cmd); err != nil {
		return fmt.Errorf("纳管主机 %s 失败: %w", hostname, err)
	}
	if err := s.upsertHostRow(ctx, id, in, hostname, sv.Host); err != nil {
		return err
	}
	for _, d := range in.Devices {
		if err := s.addSingleOSD(ctx, cl, hostname, d); err != nil {
			return err
		}
	}
	return nil
}

// upsertHostRow 落库新增主机意图行(含 OSD 意图;双方言 upsert)。
func (s *service) upsertHostRow(ctx context.Context, clusterID string, in AddHostInput, hostname, addr string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ceph: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	devices, _ := json.Marshal(in.Devices)
	d := store.DialectOf(s.db)
	suffix := store.UpsertAssignSuffix(d,
		[]string{"cluster_id", "server_id"},
		[]string{
			"labels = " + store.Excluded(d, "labels"),
			"devices = " + store.Excluded(d, "devices"),
			"hostname = " + store.Excluded(d, "hostname"),
			"addr = " + store.Excluded(d, "addr"),
		})
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO ceph_hosts (cluster_id, server_id, roles, labels, devices, hostname, addr, added_at)
		 VALUES (?, ?, 'osd', ?, ?, ?, ?, ?) `+suffix,
		clusterID, in.ServerID, strings.Join(in.Labels, ","), string(devices), hostname, addr, now,
	); err != nil {
		return fmt.Errorf("ceph: upsert host row: %w", err)
	}
	for _, d := range in.Devices {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO ceph_osds (cluster_id, host, device, osd_id, state, created_at)
			 VALUES (?, ?, ?, -1, 'pending', ?)`,
			clusterID, hostname, d, now,
		); err != nil {
			return fmt.Errorf("ceph: insert osd intent: %w", err)
		}
	}
	return tx.Commit()
}

// RemoveHost 安全移除主机:drain(迁走数据,幂等可重试)→ orch host rm → 删登记行。
func (s *service) RemoveHost(ctx context.Context, id string, hostname string) error {
	cl, err := s.readyCluster(ctx, id)
	if err != nil {
		return err
	}
	if !nameRe.MatchString(hostname) {
		return fmt.Errorf("%w: 非法主机名 %q", ErrInvalidInput, hostname)
	}
	if h, err := s.getHostRowByHostname(ctx, id, hostname); err == nil && h.ServerID == cl.BootstrapServerID {
		return fmt.Errorf("%w: bootstrap 主机不可移除(集群管理入口在其上)", ErrInvalidInput)
	}
	// drain:迁走该主机全部 OSD(幂等;数据量大时超时 → 提示稍后重试续走)。
	if _, err := s.runDrain(ctx, cl, hostname); err != nil {
		return err
	}
	if _, err := s.run(ctx, cl, []string{"ceph", "orch", "host", "rm", hostname}); err != nil {
		return fmt.Errorf("移除主机 %s 失败: %w", hostname, err)
	}
	if h, err := s.getHostRowByHostname(ctx, id, hostname); err == nil {
		return s.deleteHostRow(ctx, id, h.ServerID)
	}
	return nil
}

// runDrain 执行 orch host drain 并等待该主机 OSD 清零。
func (s *service) runDrain(ctx context.Context, cl *Cluster, hostname string) (bool, error) {
	if _, err := s.run(ctx, cl, []string{"ceph", "orch", "host", "drain", hostname}); err != nil {
		// 主机已无 OSD 时 drain 报「no osds」类错误,视为完成。
		return false, nil
	}
	deadline := time.Now().Add(drainWaitTO)
	for {
		if res, err := s.run(ctx, cl, []string{"ceph", "osd", "metadata", "-f", "json"}); err == nil {
			remaining := 0
			for _, e := range asArr(parseJSON(res.Stdout)) {
				if o := asObj(e); o != nil && pickStr(o, "hostname", "host") == hostname {
					remaining++
				}
			}
			if remaining == 0 {
				return true, nil
			}
		}
		if time.Now().After(deadline) {
			return false, fmt.Errorf("%w: 主机 %s 数据迁移未完成(已等待 %s);drain 幂等,请稍后重试移除", ErrExec, hostname, drainWaitTO)
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// getHostRowByHostname 按 orch 主机名反查意图行。
func (s *service) getHostRowByHostname(ctx context.Context, clusterID, hostname string) (*HostSpec, error) {
	rows, err := s.listHostRows(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].Hostname == hostname {
			return &rows[i], nil
		}
	}
	return nil, ErrNotFound
}

// ── OSD ──────────────────────────────────────────────────────────────────────

// ListOSDs 返回 OSD 实时列表(osd df + metadata 合并;设备列优先取 metadata)。
func (s *service) ListOSDs(ctx context.Context, id string) ([]OSD, error) {
	cl, err := s.readyCluster(ctx, id)
	if err != nil {
		return nil, err
	}
	out, err := s.runOut(ctx, cl, []string{"ceph", "osd", "df", "-f", "json"})
	if err != nil {
		return nil, err
	}
	osds := parseOSDDf(out)
	if res, err := s.run(ctx, cl, []string{"ceph", "osd", "metadata", "-f", "json"}); err == nil {
		parseOSDMetadata(res.Stdout, osds)
	}
	list := make([]OSD, 0, len(osds))
	for _, o := range osds {
		list = append(list, o)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	return list, nil
}

// AddOSD 给已纳管主机追加一块 OSD。
func (s *service) AddOSD(ctx context.Context, id string, in AddOSDInput) error {
	cl, err := s.readyCluster(ctx, id)
	if err != nil {
		return err
	}
	if !nameRe.MatchString(in.Host) {
		return fmt.Errorf("%w: 非法主机名 %q", ErrInvalidInput, in.Host)
	}
	if !deviceRe.MatchString(in.Device) {
		return fmt.Errorf("%w: 非法设备路径 %q", ErrInvalidInput, in.Device)
	}
	if err := s.addSingleOSD(ctx, cl, in.Host, in.Device); err != nil {
		return err
	}
	return s.upsertOSDRow(ctx, id, in.Host, in.Device)
}

// addSingleOSD 执行单盘 orch daemon add osd 并回填意图行。
func (s *service) addSingleOSD(ctx context.Context, cl *Cluster, host, device string) error {
	res, err := s.run(ctx, cl, []string{"ceph", "orch", "daemon", "add", "osd", host + ":" + device})
	if err != nil {
		return fmt.Errorf("添加 OSD %s:%s 失败: %w", host, device, err)
	}
	osdID := int64(-1)
	if m := osdCreatedRe.FindStringSubmatch(res.Stdout + res.Stderr); m != nil {
		osdID, _ = strconv.ParseInt(m[1], 10, 64)
	}
	_ = s.setOSDResult(ctx, cl.ID, host, device, "ok", osdID)
	return nil
}

// RemoveOSD 安全移除:osd out → 轮询 safe-to-destroy → purge → 删意图行。
func (s *service) RemoveOSD(ctx context.Context, id string, osdID int64) error {
	cl, err := s.readyCluster(ctx, id)
	if err != nil {
		return err
	}
	if osdID < 0 {
		return fmt.Errorf("%w: 非法 OSD 编号", ErrInvalidInput)
	}
	osd := "osd." + strconv.FormatInt(osdID, 10)
	if _, err := s.run(ctx, cl, []string{"ceph", "osd", "out", osd}); err != nil {
		return fmt.Errorf("osd out %s 失败: %w", osd, err)
	}
	// 等待数据迁完(safe-to-destroy;幂等,超时提示稍后重试)。
	deadline := time.Now().Add(drainWaitTO)
	for {
		res, err := s.run(ctx, cl, []string{"ceph", "osd", "safe-to-destroy", osd})
		if err == nil && strings.Contains(res.Stdout, "OK to destroy") {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: OSD %s 数据迁移未完成(已等待 %s);操作幂等,请稍后重试", ErrExec, osd, drainWaitTO)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
	if _, err := s.run(ctx, cl, []string{"ceph", "osd", "purge", osd, "--yes-i-really-really-mean-it"}); err != nil {
		return fmt.Errorf("purge %s 失败: %w", osd, err)
	}
	// 反查意图行清理(host+device 未知时静默,实时列表已不含该 OSD)。
	if h, d, ok := s.findOSDRowByOSDID(ctx, id, osdID); ok {
		_ = s.deleteOSDRowByDevice(ctx, id, h, d)
	}
	return nil
}

// findOSDRowByOSDID 按回填的 osd_id 反查意图行(辅助 RemoveOSD 清理登记)。
func (s *service) findOSDRowByOSDID(ctx context.Context, clusterID string, osdID int64) (host, device string, ok bool) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT host, device FROM ceph_osds WHERE cluster_id = ? AND osd_id = ?`, clusterID, osdID)
	if err != nil {
		return "", "", false
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		if err := rows.Scan(&host, &device); err == nil {
			return host, device, true
		}
	}
	return "", "", false
}

// ── 存储池 ───────────────────────────────────────────────────────────────────

// ListPools 返回存储池实时列表。
func (s *service) ListPools(ctx context.Context, id string) ([]Pool, error) {
	cl, err := s.readyCluster(ctx, id)
	if err != nil {
		return nil, err
	}
	out, err := s.runOut(ctx, cl, []string{"ceph", "osd", "pool", "ls", "detail", "-f", "json"})
	if err != nil {
		return nil, err
	}
	return parsePools(out), nil
}

// CreatePool 创建存储池(副本/EC;可选应用标签)。
func (s *service) CreatePool(ctx context.Context, id string, in PoolCreateInput) error {
	cl, err := s.readyCluster(ctx, id)
	if err != nil {
		return err
	}
	if !nameRe.MatchString(in.Name) {
		return fmt.Errorf("%w: 池名仅允许字母数字开头,含字母数字/点/横线/下划线", ErrInvalidInput)
	}
	if in.PGNum < 1 || in.PGNum > 65536 {
		return fmt.Errorf("%w: PG 数须在 1..65536", ErrInvalidInput)
	}
	pg := strconv.Itoa(in.PGNum)
	switch in.Type {
	case "", "replicated":
		cmd := []string{"ceph", "osd", "pool", "create", in.Name, pg}
		if _, err := s.run(ctx, cl, cmd); err != nil {
			return fmt.Errorf("创建池 %s 失败: %w", in.Name, err)
		}
		if in.Size > 1 {
			if _, err := s.run(ctx, cl, []string{"ceph", "osd", "pool", "set", in.Name, "size", strconv.Itoa(in.Size)}); err != nil {
				return fmt.Errorf("设置池 %s 副本数失败: %w", in.Name, err)
			}
		}
	case "erasure":
		cmd := []string{"ceph", "osd", "pool", "create", in.Name, pg, "erasure"}
		if in.ECProfile != "" {
			if !nameRe.MatchString(in.ECProfile) {
				return fmt.Errorf("%w: 非法 EC profile 名", ErrInvalidInput)
			}
			cmd = append(cmd, in.ECProfile)
		}
		if _, err := s.run(ctx, cl, cmd); err != nil {
			return fmt.Errorf("创建 EC 池 %s 失败: %w", in.Name, err)
		}
	default:
		return fmt.Errorf("%w: 池类型仅 replicated/erasure", ErrInvalidInput)
	}
	for _, app := range in.Apps {
		if !allowedApps[app] {
			return fmt.Errorf("%w: 应用标签仅 rbd/cephfs/rgw", ErrInvalidInput)
		}
		if _, err := s.run(ctx, cl, []string{"ceph", "osd", "pool", "application", "enable", in.Name, app}); err != nil {
			return fmt.Errorf("启用应用 %s 失败: %w", app, err)
		}
	}
	return nil
}

// UpdatePool 编辑配额(0=清除)/副本数。
func (s *service) UpdatePool(ctx context.Context, id string, name string, in PoolUpdateInput) error {
	cl, err := s.readyCluster(ctx, id)
	if err != nil {
		return err
	}
	if !nameRe.MatchString(name) {
		return fmt.Errorf("%w: 非法池名 %q", ErrInvalidInput, name)
	}
	if in.QuotaMaxBytes != nil {
		if _, err := s.run(ctx, cl, []string{"ceph", "osd", "pool", "set", name, "quota_max_bytes", strconv.FormatUint(*in.QuotaMaxBytes, 10)}); err != nil {
			return fmt.Errorf("设置池 %s 配额失败: %w", name, err)
		}
	}
	if in.Size != nil {
		if *in.Size < 2 || *in.Size > 10 {
			return fmt.Errorf("%w: 副本数须在 2..10", ErrInvalidInput)
		}
		if _, err := s.run(ctx, cl, []string{"ceph", "osd", "pool", "set", name, "size", strconv.Itoa(*in.Size)}); err != nil {
			return fmt.Errorf("设置池 %s 副本数失败: %w", name, err)
		}
	}
	return nil
}

// DeletePool 删除存储池(ceph 双确认旗标)。
func (s *service) DeletePool(ctx context.Context, id string, name string) error {
	cl, err := s.readyCluster(ctx, id)
	if err != nil {
		return err
	}
	if !nameRe.MatchString(name) {
		return fmt.Errorf("%w: 非法池名 %q", ErrInvalidInput, name)
	}
	if _, err := s.run(ctx, cl, []string{"ceph", "osd", "pool", "delete", name, name, "--yes-i-really-really-mean-it"}); err != nil {
		return fmt.Errorf("删除池 %s 失败: %w", name, err)
	}
	return nil
}

// ── RBD ──────────────────────────────────────────────────────────────────────

// validatePoolImage 校验池/镜像名并返回 "pool/image" 复合参数。
func validatePoolImage(pool, image string) (string, error) {
	if !nameRe.MatchString(pool) {
		return "", fmt.Errorf("%w: 非法池名 %q", ErrInvalidInput, pool)
	}
	if !nameRe.MatchString(image) {
		return "", fmt.Errorf("%w: 非法镜像名 %q", ErrInvalidInput, image)
	}
	return pool + "/" + image, nil
}

// ListImages 返回池内 RBD 镜像列表。
func (s *service) ListImages(ctx context.Context, id string, pool string) ([]RBDImage, error) {
	cl, err := s.readyCluster(ctx, id)
	if err != nil {
		return nil, err
	}
	if !nameRe.MatchString(pool) {
		return nil, fmt.Errorf("%w: 非法池名 %q", ErrInvalidInput, pool)
	}
	res, err := s.runOut(ctx, cl, []string{"rbd", "ls", "-l", "-p", pool, "-f", "json"})
	if err != nil {
		return nil, err
	}
	return parseImages(res, pool), nil
}

// CreateImage 创建镜像(大小以 GiB 计)。
func (s *service) CreateImage(ctx context.Context, id string, pool string, in ImageCreateInput) error {
	cl, err := s.readyCluster(ctx, id)
	if err != nil {
		return err
	}
	if _, err := validatePoolImage(pool, in.Name); err != nil {
		return err
	}
	if in.SizeGB < 1 || in.SizeGB > 1024*1024 {
		return fmt.Errorf("%w: 镜像大小须在 1..1048576 GiB", ErrInvalidInput)
	}
	if _, err := s.run(ctx, cl, []string{"rbd", "create", in.Name, "-p", pool, "--size", strconv.FormatInt(in.SizeGB, 10) + "G"}); err != nil {
		return fmt.Errorf("创建镜像 %s/%s 失败: %w", pool, in.Name, err)
	}
	return nil
}

// ResizeImage 调整镜像大小(缩容须显式 allowShrink)。
func (s *service) ResizeImage(ctx context.Context, id string, pool string, image string, in ImageResizeInput) error {
	cl, err := s.readyCluster(ctx, id)
	if err != nil {
		return err
	}
	if _, err := validatePoolImage(pool, image); err != nil {
		return err
	}
	if in.SizeGB < 1 {
		return fmt.Errorf("%w: 镜像大小须 ≥1 GiB", ErrInvalidInput)
	}
	cmd := []string{"rbd", "resize", image, "-p", pool, "--size", strconv.FormatInt(in.SizeGB, 10) + "G"}
	if in.AllowShrink {
		cmd = append(cmd, "--allow-shrink")
	}
	if _, err := s.run(ctx, cl, cmd); err != nil {
		return fmt.Errorf("调整镜像 %s/%s 失败: %w", pool, image, err)
	}
	return nil
}

// DeleteImage 删除镜像(存在快照/克隆时 rbd 会报错,错误体透出)。
func (s *service) DeleteImage(ctx context.Context, id string, pool string, image string) error {
	cl, err := s.readyCluster(ctx, id)
	if err != nil {
		return err
	}
	if _, err := validatePoolImage(pool, image); err != nil {
		return err
	}
	if _, err := s.run(ctx, cl, []string{"rbd", "rm", image, "-p", pool}); err != nil {
		return fmt.Errorf("删除镜像 %s/%s 失败: %w", pool, image, err)
	}
	return nil
}

// ListSnapshots 返回镜像快照列表。
func (s *service) ListSnapshots(ctx context.Context, id string, pool string, image string) ([]RBDSnapshot, error) {
	cl, err := s.readyCluster(ctx, id)
	if err != nil {
		return nil, err
	}
	spec, err := validatePoolImage(pool, image)
	if err != nil {
		return nil, err
	}
	res, err := s.runOut(ctx, cl, []string{"rbd", "snap", "ls", spec, "-f", "json"})
	if err != nil {
		return nil, err
	}
	return parseSnapshots(res), nil
}

// CreateSnapshot 创建快照。
func (s *service) CreateSnapshot(ctx context.Context, id string, pool string, image string, snap string) error {
	cl, err := s.readyCluster(ctx, id)
	if err != nil {
		return err
	}
	spec, err := validatePoolImage(pool, image)
	if err != nil {
		return err
	}
	if !nameRe.MatchString(snap) {
		return fmt.Errorf("%w: 非法快照名 %q", ErrInvalidInput, snap)
	}
	if _, err := s.run(ctx, cl, []string{"rbd", "snap", "create", spec + "@" + snap}); err != nil {
		return fmt.Errorf("创建快照 %s@%s 失败: %w", spec, snap, err)
	}
	return nil
}

// DeleteSnapshot 删除快照。
func (s *service) DeleteSnapshot(ctx context.Context, id string, pool string, image string, snap string) error {
	cl, err := s.readyCluster(ctx, id)
	if err != nil {
		return err
	}
	spec, err := validatePoolImage(pool, image)
	if err != nil {
		return err
	}
	if !nameRe.MatchString(snap) {
		return fmt.Errorf("%w: 非法快照名 %q", ErrInvalidInput, snap)
	}
	if _, err := s.run(ctx, cl, []string{"rbd", "snap", "rm", spec + "@" + snap}); err != nil {
		return fmt.Errorf("删除快照 %s@%s 失败: %w", spec, snap, err)
	}
	return nil
}

// RollbackSnapshot 把镜像回滚到快照(快照之后的数据丢失;前端须强确认)。
func (s *service) RollbackSnapshot(ctx context.Context, id string, pool string, image string, snap string) error {
	cl, err := s.readyCluster(ctx, id)
	if err != nil {
		return err
	}
	spec, err := validatePoolImage(pool, image)
	if err != nil {
		return err
	}
	if !nameRe.MatchString(snap) {
		return fmt.Errorf("%w: 非法快照名 %q", ErrInvalidInput, snap)
	}
	if _, err := s.run(ctx, cl, []string{"rbd", "snap", "rollback", spec + "@" + snap}); err != nil {
		return fmt.Errorf("回滚 %s@%s 失败: %w", spec, snap, err)
	}
	return nil
}

// ── CephFS ───────────────────────────────────────────────────────────────────

// ListFilesystems 返回 CephFS 列表(MDS 数经 fs status 尽力而为)。
func (s *service) ListFilesystems(ctx context.Context, id string) ([]Filesystem, error) {
	cl, err := s.readyCluster(ctx, id)
	if err != nil {
		return nil, err
	}
	res, err := s.runOut(ctx, cl, []string{"ceph", "fs", "ls", "-f", "json"})
	if err != nil {
		return nil, err
	}
	fss := parseFilesystems(res)
	if res2, err := s.run(ctx, cl, []string{"ceph", "fs", "status", "-f", "json"}); err == nil {
		n := parseMdsCount(res2.Stdout)
		for i := range fss {
			fss[i].MdsCount = n
		}
	}
	return fss, nil
}

// ListSubvolumes 返回 fs 下子卷列表(名称 + 路径 + 用量;≤50 个)。
func (s *service) ListSubvolumes(ctx context.Context, id string, fs string) ([]Subvolume, error) {
	cl, err := s.readyCluster(ctx, id)
	if err != nil {
		return nil, err
	}
	if !nameRe.MatchString(fs) {
		return nil, fmt.Errorf("%w: 非法文件系统名 %q", ErrInvalidInput, fs)
	}
	res, err := s.runOut(ctx, cl, []string{"ceph", "fs", "subvolumels", fs, "-f", "json"})
	if err != nil {
		return nil, err
	}
	names := parseSubvolumeNames(res)
	out := make([]Subvolume, 0, len(names))
	for i, name := range names {
		if i >= 50 {
			break
		}
		sv := Subvolume{Name: name}
		if res, err := s.run(ctx, cl, []string{"ceph", "fs", "subvolume", "getpath", fs, name}); err == nil {
			sv.Path = strings.TrimSpace(res.Stdout)
		}
		if res, err := s.run(ctx, cl, []string{"ceph", "fs", "subvolume", "info", fs, name, "-f", "json"}); err == nil {
			sv.BytesUsed, sv.QuotaBytes = parseSubvolumeInfo(res.Stdout)
		}
		out = append(out, sv)
	}
	return out, nil
}

// CreateSubvolume 创建子卷(sizeGb 0 = 不限额)。
func (s *service) CreateSubvolume(ctx context.Context, id string, fs string, in SubvolumeCreateInput) error {
	cl, err := s.readyCluster(ctx, id)
	if err != nil {
		return err
	}
	if !nameRe.MatchString(fs) {
		return fmt.Errorf("%w: 非法文件系统名 %q", ErrInvalidInput, fs)
	}
	if !nameRe.MatchString(in.Name) {
		return fmt.Errorf("%w: 非法子卷名 %q", ErrInvalidInput, in.Name)
	}
	if in.SizeGB < 0 || in.SizeGB > 1024*1024 {
		return fmt.Errorf("%w: 子卷大小须在 0(不限)..1048576 GiB", ErrInvalidInput)
	}
	cmd := []string{"ceph", "fs", "subvolume", "create", fs, in.Name}
	if in.SizeGB > 0 {
		cmd = append(cmd, "--size", strconv.FormatInt(in.SizeGB*1024*1024*1024, 10))
	}
	if _, err := s.run(ctx, cl, cmd); err != nil {
		return fmt.Errorf("创建子卷 %s/%s 失败: %w", fs, in.Name, err)
	}
	return nil
}

// DeleteSubvolume 删除子卷(存在快照时 ceph 会报错,错误体透出)。
func (s *service) DeleteSubvolume(ctx context.Context, id string, fs string, name string) error {
	cl, err := s.readyCluster(ctx, id)
	if err != nil {
		return err
	}
	if !nameRe.MatchString(fs) || !nameRe.MatchString(name) {
		return fmt.Errorf("%w: 非法文件系统/子卷名", ErrInvalidInput)
	}
	if _, err := s.run(ctx, cl, []string{"ceph", "fs", "subvolume", "rm", fs, name}); err != nil {
		return fmt.Errorf("删除子卷 %s/%s 失败: %w", fs, name, err)
	}
	return nil
}
