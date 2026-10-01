package servicereg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/huangchengsir/pipewright/internal/store"
)

// settingsID 是 service_reg_settings 的单行主键(全库仅此一行)。
const settingsID = "default"

// 默认值(New/迁移之外,代码层的兜底单例)。
const (
	defaultNginxImage    = "nginx:stable-alpine"
	defaultNetwork       = "pipewright-gateway"
	defaultContainerName = "pipewright-nginx"
	defaultVolumeName    = "pipewright_nginx"
)

// storedSettings 是 settings 的内部存储表示(目前与 Settings 同形,保留包装以便将来扩展)。
type storedSettings struct {
	Settings
}

// Store 持久化服务注册网关(参数化 SQL,sqlite/mysql 两方言一致)。
type Store struct {
	db *sql.DB
}

// NewStore 构造持久层。
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// ---------- settings ----------

// getOrCreateSettings 读取网关设置单例;不存在则插入默认行后回读(幂等)。
func (s *Store) getOrCreateSettings(ctx context.Context) (*Settings, error) {
	st, err := s.getSettings(ctx)
	if err == nil {
		return &st.Settings, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, ierr := s.db.ExecContext(ctx,
		`INSERT INTO service_reg_settings
		   (id, server_id, http_port, https_port, image, network, container_name, volume_name,
		    last_apply_at, last_apply_error, created_at, updated_at)
		 VALUES (?, '', 80, 443, ?, ?, ?, ?, '', '', ?, ?)`,
		settingsID, defaultNginxImage, defaultNetwork, defaultContainerName, defaultVolumeName, now, now,
	)
	if ierr != nil {
		// 并发初始化撞唯一键 → 回读即可。
		if !store.IsUniqueErr(ierr) {
			return nil, fmt.Errorf("servicereg: insert settings: %w", ierr)
		}
	}
	st, err = s.getSettings(ctx)
	if err != nil {
		return nil, err
	}
	return &st.Settings, nil
}

// getSettings 读取设置单例;不存在 → ErrNotFound。
func (s *Store) getSettings(ctx context.Context) (*storedSettings, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT server_id, http_port, https_port, image, network, container_name, volume_name,
		        last_apply_at, last_apply_error, created_at, updated_at
		 FROM service_reg_settings WHERE id = ?`, settingsID)
	return scanSettings(row)
}

// updateSettings 落库设置(全部列;调用方已校验)。
func (s *Store) updateSettings(ctx context.Context, st *Settings) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx,
		`UPDATE service_reg_settings
		 SET server_id = ?, http_port = ?, https_port = ?, image = ?, network = ?, container_name = ?,
		     volume_name = ?, updated_at = ?
		 WHERE id = ?`,
		st.ServerID, st.HTTPPort, st.HTTPSPort, st.Image, st.Network, st.ContainerName,
		st.VolumeName, now, settingsID,
	)
	if err != nil {
		return fmt.Errorf("servicereg: update settings: %w", err)
	}
	return nil
}

// setApplyResult 回写最近一次编排结果(成功 errText 为空)。
func (s *Store) setApplyResult(ctx context.Context, at time.Time, errText string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE service_reg_settings SET last_apply_at = ?, last_apply_error = ?, updated_at = ? WHERE id = ?`,
		at.UTC().Format(time.RFC3339), truncate(errText, 2000), at.UTC().Format(time.RFC3339), settingsID)
	if err != nil {
		return fmt.Errorf("servicereg: set apply result: %w", err)
	}
	return nil
}

func scanSettings(sc scanner) (*storedSettings, error) {
	var (
		st          storedSettings
		lastApply   sql.NullString
		lastErr     sql.NullString
		createdStr  string
		updatedStr  string
	)
	if err := sc.Scan(
		&st.ServerID, &st.HTTPPort, &st.HTTPSPort, &st.Image, &st.Network, &st.ContainerName,
		&st.VolumeName, &lastApply, &lastErr, &createdStr, &updatedStr,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if lastApply.String != "" {
		if t, err := time.Parse(time.RFC3339, lastApply.String); err == nil {
			st.LastApplyAt = t
		}
	}
	st.LastApplyError = lastErr.String
	st.CreatedAt = parseTime(createdStr)
	st.UpdatedAt = parseTime(updatedStr)
	return &st, nil
}

// ---------- domains ----------

// insertDomain 落库基域;base_domain 唯一冲突 → ErrDomainTaken。
func (s *Store) insertDomain(ctx context.Context, d *Domain) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO service_reg_domains (id, base_domain, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		d.ID, d.BaseDomain, d.CreatedAt.UTC().Format(time.RFC3339), d.UpdatedAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		if store.IsUniqueErr(err) {
			return ErrDomainTaken
		}
		return fmt.Errorf("servicereg: insert domain: %w", err)
	}
	return nil
}

// getDomain 读取单个基域;不存在 → ErrNotFound。
func (s *Store) getDomain(ctx context.Context, id string) (*Domain, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, base_domain, cert_subject, cert_expires_at,
		        cert_pem_sealed IS NOT NULL AND LENGTH(cert_pem_sealed) > 0, created_at, updated_at
		 FROM service_reg_domains WHERE id = ?`, id)
	d, err := scanDomain(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return d, nil
}

// listDomains 返回全部基域(创建时间倒序)。
func (s *Store) listDomains(ctx context.Context) ([]Domain, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, base_domain, cert_subject, cert_expires_at,
		        cert_pem_sealed IS NOT NULL AND LENGTH(cert_pem_sealed) > 0, created_at, updated_at
		 FROM service_reg_domains ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("servicereg: list domains: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]Domain, 0)
	for rows.Next() {
		d, err := scanDomain(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// updateCert 落库证书密文 + 展示元数据;不存在 → ErrNotFound。
func (s *Store) updateCert(ctx context.Context, domainID string, sealedCert, sealedKey []byte, subject, dnsNames string, expires time.Time) (*Domain, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx,
		`UPDATE service_reg_domains
		 SET cert_pem_sealed = ?, key_pem_sealed = ?, cert_subject = ?, cert_expires_at = ?, updated_at = ?
		 WHERE id = ?`,
		sealedCert, sealedKey, subject+", SAN:"+dnsNames, expires.Format(time.RFC3339), now, domainID,
	)
	if err != nil {
		return nil, fmt.Errorf("servicereg: update cert: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return s.getDomain(ctx, domainID)
}

// clearCert 清空某基域证书(密文/元数据置空);行不存在 → ErrNotFound。
func (s *Store) clearCert(ctx context.Context, domainID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx,
		`UPDATE service_reg_domains
		 SET cert_pem_sealed = NULL, key_pem_sealed = NULL, cert_subject = '', cert_expires_at = '', updated_at = ?
		 WHERE id = ?`, now, domainID)
	if err != nil {
		return fmt.Errorf("servicereg: clear cert: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// getCertSealed 返回某基域的证书密文对(apply 下发时进程内解密;无证书 → ok=false)。
func (s *Store) getCertSealed(ctx context.Context, domainID string) (cert, key []byte, ok bool, err error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT cert_pem_sealed, key_pem_sealed FROM service_reg_domains WHERE id = ?`, domainID)
	var c, k []byte
	if err := row.Scan(&c, &k); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, false, ErrNotFound
		}
		return nil, nil, false, fmt.Errorf("servicereg: read cert: %w", err)
	}
	if len(c) == 0 || len(k) == 0 {
		return nil, nil, false, nil
	}
	return c, k, true, nil
}

// deleteDomain 删除基域及其下全部服务(配置层级联;apply 随后收敛);不存在 → ErrNotFound。
func (s *Store) deleteDomain(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM service_reg_domains WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("servicereg: delete domain: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM service_reg_services WHERE domain_id = ?`, id); err != nil {
		return fmt.Errorf("servicereg: cascade services: %w", err)
	}
	return nil
}

func scanDomain(sc scanner) (*Domain, error) {
	var (
		d         Domain
		hasCert   bool
		expires   sql.NullString
		createdS  string
		updatedS  string
	)
	if err := sc.Scan(&d.ID, &d.BaseDomain, &d.CertSubject, &expires, &hasCert, &createdS, &updatedS); err != nil {
		return nil, err
	}
	d.HasCert = hasCert
	if expires.String != "" {
		if t, err := time.Parse(time.RFC3339, expires.String); err == nil {
			d.CertExpiresAt = t
		}
	}
	d.CreatedAt = parseTime(createdS)
	d.UpdatedAt = parseTime(updatedS)
	return &d, nil
}

// ---------- services ----------

// insertService 落库服务;(domain_id,name) 唯一冲突 → ErrServiceTaken。
func (s *Store) insertService(ctx context.Context, svc *RegisteredService) error {
	enabled := 0
	if svc.Enabled {
		enabled = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO service_reg_services
		   (id, domain_id, name, protocol, upstream_kind, upstream, upstream_port, tcp_listen_port,
		    enabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		svc.ID, svc.DomainID, svc.Name, svc.Protocol, svc.UpstreamKind, svc.Upstream, svc.UpstreamPort,
		svc.TCPListenPort, enabled, svc.CreatedAt.UTC().Format(time.RFC3339), svc.UpdatedAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		if store.IsUniqueErr(err) {
			return ErrServiceTaken
		}
		return fmt.Errorf("servicereg: insert service: %w", err)
	}
	return nil
}

// getService 读取单个服务;不存在 → ErrNotFound。
func (s *Store) getService(ctx context.Context, id string) (*RegisteredService, error) {
	row := s.db.QueryRowContext(ctx, serviceCols+` WHERE id = ?`, id)
	svc, err := scanService(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return svc, nil
}

const serviceCols = `SELECT id, domain_id, name, protocol, upstream_kind, upstream, upstream_port,
       tcp_listen_port, enabled, created_at, updated_at FROM service_reg_services`

// listServices 返回全部服务(创建时间倒序)。
func (s *Store) listServices(ctx context.Context) ([]RegisteredService, error) {
	rows, err := s.db.QueryContext(ctx, serviceCols+` ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("servicereg: list services: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]RegisteredService, 0)
	for rows.Next() {
		svc, err := scanService(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *svc)
	}
	return out, rows.Err()
}

// listServicesWithDomain 返回全部服务 + 所属基域名(join 展示用)。
func (s *Store) listServicesWithDomain(ctx context.Context) ([]ServiceWithDomain, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT s.id, s.domain_id, s.name, s.protocol, s.upstream_kind, s.upstream, s.upstream_port,
		        s.tcp_listen_port, s.enabled, s.created_at, s.updated_at, d.base_domain
		 FROM service_reg_services s
		 JOIN service_reg_domains d ON d.id = s.domain_id
		 ORDER BY s.created_at DESC, s.id`)
	if err != nil {
		return nil, fmt.Errorf("servicereg: list services with domain: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]ServiceWithDomain, 0)
	for rows.Next() {
		var (
			swd       ServiceWithDomain
			enabled   int
			createdS  string
			updatedS  string
		)
		if err := rows.Scan(
			&swd.ID, &swd.DomainID, &swd.Name, &swd.Protocol, &swd.UpstreamKind, &swd.Upstream,
			&swd.UpstreamPort, &swd.TCPListenPort, &enabled, &createdS, &updatedS, &swd.BaseDomain,
		); err != nil {
			return nil, err
		}
		swd.Enabled = enabled != 0
		swd.CreatedAt = parseTime(createdS)
		swd.UpdatedAt = parseTime(updatedS)
		out = append(out, swd)
	}
	return out, rows.Err()
}

// updateService 全量更新服务列(调用方已校验);不存在 → ErrNotFound。
func (s *Store) updateService(ctx context.Context, svc *RegisteredService) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE service_reg_services
		 SET name = ?, protocol = ?, upstream_kind = ?, upstream = ?, upstream_port = ?,
		     tcp_listen_port = ?, updated_at = ?
		 WHERE id = ?`,
		svc.Name, svc.Protocol, svc.UpstreamKind, svc.Upstream, svc.UpstreamPort,
		svc.TCPListenPort, svc.UpdatedAt.UTC().Format(time.RFC3339), svc.ID,
	)
	if err != nil {
		if store.IsUniqueErr(err) {
			return ErrServiceTaken
		}
		return fmt.Errorf("servicereg: update service: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// setServiceEnabled 启停服务;不存在 → ErrNotFound。
func (s *Store) setServiceEnabled(ctx context.Context, id string, on bool) error {
	v := 0
	if on {
		v = 1
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE service_reg_services SET enabled = ?, updated_at = ? WHERE id = ?`,
		v, time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return fmt.Errorf("servicereg: set enabled: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// deleteService 删除服务;不存在 → ErrNotFound。
func (s *Store) deleteService(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM service_reg_services WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("servicereg: delete service: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanService(sc scanner) (*RegisteredService, error) {
	var (
		svc       RegisteredService
		enabled   int
		createdS  string
		updatedS  string
	)
	if err := sc.Scan(
		&svc.ID, &svc.DomainID, &svc.Name, &svc.Protocol, &svc.UpstreamKind, &svc.Upstream,
		&svc.UpstreamPort, &svc.TCPListenPort, &enabled, &createdS, &updatedS,
	); err != nil {
		return nil, err
	}
	svc.Enabled = enabled != 0
	svc.CreatedAt = parseTime(createdS)
	svc.UpdatedAt = parseTime(updatedS)
	return &svc, nil
}

// scanner 抽象 *sql.Row 与 *sql.Rows 的 Scan。
type scanner interface {
	Scan(dest ...any) error
}

// ---------- instances(0051) ----------

const instanceCols = `SELECT id, service_id, container, port, attached, created_at, updated_at
FROM service_reg_instances`

// insertInstance 落库实例;(service_id, container) 唯一冲突 → ErrInstanceTaken。
func (s *Store) insertInstance(ctx context.Context, i *Instance) error {
	attached := 0
	if i.Attached {
		attached = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO service_reg_instances (id, service_id, container, port, attached, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		i.ID, i.ServiceID, i.Container, i.Port, attached,
		i.CreatedAt.UTC().Format(time.RFC3339), i.UpdatedAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		if store.IsUniqueErr(err) {
			return ErrInstanceTaken
		}
		return fmt.Errorf("servicereg: insert instance: %w", err)
	}
	return nil
}

// getInstance 读取单个实例;不存在 → ErrNotFound。
func (s *Store) getInstance(ctx context.Context, id string) (*Instance, error) {
	row := s.db.QueryRowContext(ctx, instanceCols+` WHERE id = ?`, id)
	i, err := scanInstance(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return i, nil
}

// getInstanceByContainer 按(服务, 容器名)取实例;不存在 → ErrNotFound。
func (s *Store) getInstanceByContainer(ctx context.Context, serviceID, container string) (*Instance, error) {
	row := s.db.QueryRowContext(ctx, instanceCols+` WHERE service_id = ? AND container = ?`, serviceID, container)
	i, err := scanInstance(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return i, nil
}

// listInstances 返回某服务的全部实例(创建时间正序)。
func (s *Store) listInstances(ctx context.Context, serviceID string) ([]Instance, error) {
	rows, err := s.db.QueryContext(ctx, instanceCols+` WHERE service_id = ? ORDER BY created_at ASC, id`, serviceID)
	if err != nil {
		return nil, fmt.Errorf("servicereg: list instances: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]Instance, 0)
	for rows.Next() {
		i, err := scanInstance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *i)
	}
	return out, rows.Err()
}

// listAllInstances 返回全部实例(渲染 / deploy 反查用)。
func (s *Store) listAllInstances(ctx context.Context) ([]Instance, error) {
	rows, err := s.db.QueryContext(ctx, instanceCols+` ORDER BY created_at ASC, id`)
	if err != nil {
		return nil, fmt.Errorf("servicereg: list all instances: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]Instance, 0)
	for rows.Next() {
		i, err := scanInstance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *i)
	}
	return out, rows.Err()
}

// setInstanceAttached 摘/挂实例;不存在 → ErrNotFound。
func (s *Store) setInstanceAttached(ctx context.Context, id string, attached bool) error {
	v := 0
	if attached {
		v = 1
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE service_reg_instances SET attached = ?, updated_at = ? WHERE id = ?`,
		v, time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return fmt.Errorf("servicereg: set instance attached: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// updateInstanceContainer 实例行改名(SwapInstance 用);目标名撞唯一键 → ErrInstanceTaken。
func (s *Store) updateInstanceContainer(ctx context.Context, id, container string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE service_reg_instances SET container = ?, updated_at = ? WHERE id = ?`,
		container, time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		if store.IsUniqueErr(err) {
			return ErrInstanceTaken
		}
		return fmt.Errorf("servicereg: update instance container: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// deleteInstance 删除实例;不存在 → ErrNotFound。
func (s *Store) deleteInstance(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM service_reg_instances WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("servicereg: delete instance: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanInstance(sc scanner) (*Instance, error) {
	var (
		i         Instance
		attached  int
		createdS  string
		updatedS  string
	)
	if err := sc.Scan(&i.ID, &i.ServiceID, &i.Container, &i.Port, &attached, &createdS, &updatedS); err != nil {
		return nil, err
	}
	i.Attached = attached != 0
	i.CreatedAt = parseTime(createdS)
	i.UpdatedAt = parseTime(updatedS)
	return &i, nil
}

// parseTime 解析 RFC3339 存储串(失败给零值,不炸列表)。
func parseTime(s string) time.Time {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

// truncate 截断长错误文本(入库/回显限制)。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
