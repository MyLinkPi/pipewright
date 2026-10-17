// store.go — ceph 域持久层(参数化 SQL;sqlite/mysql 两方言一致;仅本文件触表)。
package ceph

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/huangchengsir/pipewright/internal/store"
)

const clusterCols = `SELECT c.id, c.name, c.fsid, c.bootstrap_server_id,
	COALESCE((SELECT name FROM servers s WHERE s.id = c.bootstrap_server_id), ''),
	c.image, c.cephadm_source, c.public_network, c.cluster_network,
	c.state, c.dashboard_url, c.dashboard_user, c.dashboard_credential_id, c.created_at, c.updated_at
	FROM ceph_clusters c`

// insertCluster 落库集群 + 主机意图 + OSD 意图(同一事务);name 冲突 → ErrNameTaken。
func (s *service) insertCluster(ctx context.Context, cl *Cluster, hosts []HostSpec) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ceph: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO ceph_clusters
		   (id, name, fsid, bootstrap_server_id, image, cephadm_source, public_network, cluster_network,
		    state, dashboard_url, dashboard_user, dashboard_credential_id, created_at, updated_at)
		 VALUES (?, ?, '', ?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?)`,
		cl.ID, cl.Name, cl.BootstrapServerID, cl.Image, cl.CephadmSource, cl.PublicNetwork, cl.ClusterNetwork,
		cl.State, cl.DashboardUser, cl.DashboardCredentialID, now, now,
	); err != nil {
		if store.IsUniqueErr(err) {
			return ErrNameTaken
		}
		return fmt.Errorf("ceph: insert cluster: %w", err)
	}
	for _, h := range hosts {
		if err := insertHostRow(ctx, tx, cl.ID, h, now); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ceph: commit cluster: %w", err)
	}
	return nil
}

// insertHostRow 写一行主机意图 + 其 OSD 意图行(tx 内)。
func insertHostRow(ctx context.Context, tx *sql.Tx, clusterID string, h HostSpec, now string) error {
	devices, err := json.Marshal(h.Devices)
	if err != nil {
		devices = []byte("[]")
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO ceph_hosts (cluster_id, server_id, roles, labels, devices, hostname, addr, added_at)
		 VALUES (?, ?, ?, ?, ?, '', '', ?)`,
		clusterID, h.ServerID, strings.Join(h.Roles, ","), strings.Join(h.Labels, ","), string(devices), now,
	); err != nil {
		return fmt.Errorf("ceph: insert host: %w", err)
	}
	for _, d := range h.Devices {
		// host 先用 serverId 占位(创建时尚无 orch 主机名,且 serverId 唯一);
		// 部署 preflight 确定 hostname 后经 migrateOSDHost 改写为 orch 主机名。
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO ceph_osds (cluster_id, host, device, osd_id, state, created_at)
			 VALUES (?, ?, ?, -1, 'pending', ?)`,
			clusterID, h.ServerID, d, now,
		); err != nil {
			return fmt.Errorf("ceph: insert osd intent: %w", err)
		}
	}
	return nil
}

// migrateOSDHost 把 OSD 意图行的 host 键从创建期的 serverId 占位改写为 orch 主机名
// (幂等;同 host+device 已存在时先清旧行再改写,避免主键冲突)。
func (s *service) migrateOSDHost(ctx context.Context, clusterID, serverID, hostname string) error {
	if hostname == "" || hostname == serverID {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ceph: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM ceph_osds WHERE cluster_id = ? AND host = ?`, clusterID, hostname); err != nil {
		return fmt.Errorf("ceph: clear stale osd rows: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE ceph_osds SET host = ? WHERE cluster_id = ? AND host = ?`, hostname, clusterID, serverID)
	if err != nil {
		return fmt.Errorf("ceph: migrate osd host: %w", err)
	}
	_ = res
	return tx.Commit()
}

// listClusters 返回全部集群(join servers 取 bootstrap 主机名)。
func (s *service) listClusters(ctx context.Context) ([]*Cluster, error) {
	rows, err := s.db.QueryContext(ctx, clusterCols+` ORDER BY c.created_at DESC, c.id`)
	if err != nil {
		return nil, fmt.Errorf("ceph: list clusters: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []*Cluster{}
	for rows.Next() {
		cl, err := scanCluster(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, cl)
	}
	return out, rows.Err()
}

// getCluster 读取单个集群;不存在 → ErrNotFound。
func (s *service) getCluster(ctx context.Context, id string) (*Cluster, error) {
	row := s.db.QueryRowContext(ctx, clusterCols+` WHERE c.id = ?`, id)
	cl, err := scanCluster(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return cl, nil
}

func scanCluster(row interface{ Scan(...any) error }) (*Cluster, error) {
	var cl Cluster
	var created, updated string
	if err := row.Scan(&cl.ID, &cl.Name, &cl.FSID, &cl.BootstrapServerID, &cl.BootstrapServerName,
		&cl.Image, &cl.CephadmSource, &cl.PublicNetwork, &cl.ClusterNetwork,
		&cl.State, &cl.DashboardURL, &cl.DashboardUser, &cl.DashboardCredentialID, &created, &updated); err != nil {
		return nil, err
	}
	cl.CreatedAt, _ = time.Parse(time.RFC3339, created)
	cl.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
	return &cl, nil
}

// updateClusterDeploy 部署管线结束时回填 fsid/dashboard/state。
func (s *service) updateClusterDeploy(ctx context.Context, id, fsid, dashboardURL, state string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE ceph_clusters SET fsid = ?, dashboard_url = ?, state = ?, updated_at = ? WHERE id = ?`,
		fsid, dashboardURL, state, time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return fmt.Errorf("ceph: update cluster deploy: %w", err)
	}
	return nil
}

// updateClusterState 仅改状态列(部署开始/失败标记)。
func (s *service) updateClusterState(ctx context.Context, id, state string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE ceph_clusters SET state = ?, updated_at = ? WHERE id = ?`,
		state, time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return fmt.Errorf("ceph: update cluster state: %w", err)
	}
	return nil
}

// updateClusterCredential 回填 dashboard 凭据引用。
func (s *service) updateClusterCredential(ctx context.Context, id, credentialID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE ceph_clusters SET dashboard_credential_id = ?, updated_at = ? WHERE id = ?`,
		credentialID, time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return fmt.Errorf("ceph: update cluster credential: %w", err)
	}
	return nil
}

// deleteCluster 级联删除登记数据(集群 + 主机 + OSD 行)。
func (s *service) deleteCluster(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ceph: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{
		`DELETE FROM ceph_osds WHERE cluster_id = ?`,
		`DELETE FROM ceph_hosts WHERE cluster_id = ?`,
		`DELETE FROM ceph_clusters WHERE id = ?`,
	} {
		res, err := tx.ExecContext(ctx, q, id)
		if err != nil {
			return fmt.Errorf("ceph: delete cluster rows: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 && q == `DELETE FROM ceph_clusters WHERE id = ?` {
			return ErrNotFound
		}
	}
	return tx.Commit()
}

// listHostRows 读取主机意图行(部署向导预填 / 主机页合并展示)。
func (s *service) listHostRows(ctx context.Context, clusterID string) ([]HostSpec, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT server_id, roles, labels, devices, hostname, addr FROM ceph_hosts WHERE cluster_id = ? ORDER BY server_id`,
		clusterID)
	if err != nil {
		return nil, fmt.Errorf("ceph: list hosts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []HostSpec{}
	for rows.Next() {
		var h HostSpec
		var roles, labels, devices, hostname, addr string
		if err := rows.Scan(&h.ServerID, &roles, &labels, &devices, &hostname, &addr); err != nil {
			return nil, err
		}
		h.Roles = splitCSV(roles)
		h.Labels = splitCSV(labels)
		h.Devices = decodeDevices(devices)
		h.Hostname = hostname
		h.Addr = addr
		out = append(out, h)
	}
	return out, rows.Err()
}

// updateHostDeploy 部署管线回填主机 orch 名/地址。
func (s *service) updateHostDeploy(ctx context.Context, clusterID, serverID, hostname, addr string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE ceph_hosts SET hostname = ?, addr = ? WHERE cluster_id = ? AND server_id = ?`,
		hostname, addr, clusterID, serverID)
	if err != nil {
		return fmt.Errorf("ceph: update host deploy: %w", err)
	}
	return nil
}

// deleteHostRow 删除主机意图行及其 OSD 行(按 hostname 关联;hostname 空 = 尚未部署)。
func (s *service) deleteHostRow(ctx context.Context, clusterID, serverID string) error {
	h, err := s.getHostRow(ctx, clusterID, serverID)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ceph: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if h.Hostname != "" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM ceph_osds WHERE cluster_id = ? AND host = ?`, clusterID, h.Hostname); err != nil {
			return fmt.Errorf("ceph: delete osd rows: %w", err)
		}
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM ceph_hosts WHERE cluster_id = ? AND server_id = ?`, clusterID, serverID)
	if err != nil {
		return fmt.Errorf("ceph: delete host row: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// getHostRow 读取单台主机意图行。
func (s *service) getHostRow(ctx context.Context, clusterID, serverID string) (*HostSpec, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT server_id, roles, labels, devices, hostname, addr FROM ceph_hosts WHERE cluster_id = ? AND server_id = ?`,
		clusterID, serverID)
	var h HostSpec
	var roles, labels, devices, hostname, addr string
	if err := row.Scan(&h.ServerID, &roles, &labels, &devices, &hostname, &addr); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	h.Roles = splitCSV(roles)
	h.Labels = splitCSV(labels)
	h.Devices = decodeDevices(devices)
	h.Hostname = hostname
	h.Addr = addr
	return &h, nil
}

// upsertOSDRow 追加一条 OSD 意图行(host 为 orch 主机名;同 host+device 幂等重置)。
func (s *service) upsertOSDRow(ctx context.Context, clusterID, host, device string) error {
	suffix := store.UpsertAssignSuffix(store.DialectOf(s.db),
		[]string{"cluster_id", "host", "device"},
		[]string{"state = 'pending'", "osd_id = -1"})
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO ceph_osds (cluster_id, host, device, osd_id, state, created_at)
		 VALUES (?, ?, ?, -1, 'pending', ?) `+suffix,
		clusterID, host, device, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("ceph: upsert osd row: %w", err)
	}
	return nil
}

// setOSDResult 回填 OSD 部署结果(osd_id / ok|failed)。
func (s *service) setOSDResult(ctx context.Context, clusterID, host, device, state string, osdID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE ceph_osds SET osd_id = ?, state = ? WHERE cluster_id = ? AND host = ? AND device = ?`,
		osdID, state, clusterID, host, device)
	if err != nil {
		return fmt.Errorf("ceph: set osd result: %w", err)
	}
	return nil
}

// deleteOSDRowByDevice 删除 OSD 意图行。
func (s *service) deleteOSDRowByDevice(ctx context.Context, clusterID, host, device string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM ceph_osds WHERE cluster_id = ? AND host = ? AND device = ?`, clusterID, host, device)
	if err != nil {
		return fmt.Errorf("ceph: delete osd row: %w", err)
	}
	return nil
}

// listOSDRows 返回 OSD 意图行(合并展示用)。
func (s *service) listOSDRows(ctx context.Context, clusterID string) (map[string]string, error) {
	// key = host+"\x00"+device → state。
	rows, err := s.db.QueryContext(ctx,
		`SELECT host, device, state FROM ceph_osds WHERE cluster_id = ?`, clusterID)
	if err != nil {
		return nil, fmt.Errorf("ceph: list osd rows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var host, device, state string
		if err := rows.Scan(&host, &device, &state); err != nil {
			return nil, err
		}
		out[host+"\x00"+device] = state
	}
	return out, rows.Err()
}

// splitCSV 拆逗号分隔列(空串 → nil)。
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// decodeDevices 解析 devices JSON 列;异常回退空。
func decodeDevices(s string) []string {
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}
