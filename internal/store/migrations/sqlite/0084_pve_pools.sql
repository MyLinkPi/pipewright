-- 0084 pve pools(SQLite):服务器池 + PVE 集群纳管。
--   server_pools:服务器池。kind=standard(普通服务器,default 单例池,存量全部回填)或
--     pve(PVE 集群池,cluster_id 关联 pve_clusters)。池由系统管理(PVE 探测自动创建),
--     不开放手工 CRUD,也不进入部署/构建选择器语义(选择器仍按标签圈选)。
--   pve_clusters:经 SSH 探测(pveversion + corosync.conf/pvecm nodes)自动登记的 PVE 集群。
--     token_sealed 为 API Token 值经 vault.SealSecret 的密文 BLOB(绝无明文落库/出响应/进日志);
--     可为空(state=created,仅库存,管理面待 Token);
--     tls_mode:insecure(内网自签,跳过校验)| fingerprint(SHA256 指纹固定)| verify(系统 CA);
--     readonly 默认 1:电源操作在领域层拒绝,解锁是显式且留审计的独立动作;
--     state:created(已发现/未配 Token)→ ready(Token 测连通过)。
--   servers.pool_id / servers.kind:系统管理字段(kind=standard|pve_node),仅 PVE 探测流
--     写入,不进服务器更新 API。pve_node 一律不允许作为构建机与部署目标
--     (调度器/部署圈选/应用商店部署三处域层拦截)。

CREATE TABLE IF NOT EXISTS server_pools (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    kind       TEXT NOT NULL DEFAULT 'standard',
    cluster_id TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_server_pools_name ON server_pools (name);

CREATE TABLE IF NOT EXISTS pve_clusters (
    id               TEXT PRIMARY KEY,
    name             TEXT NOT NULL,
    api_url          TEXT NOT NULL DEFAULT '',
    token_name       TEXT NOT NULL DEFAULT '',
    token_sealed     BLOB,
    tls_mode         TEXT NOT NULL DEFAULT 'insecure',
    tls_fingerprint  TEXT NOT NULL DEFAULT '',
    state            TEXT NOT NULL DEFAULT 'created',
    readonly         INTEGER NOT NULL DEFAULT 1,
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_pve_clusters_name ON pve_clusters (name);

ALTER TABLE servers ADD COLUMN pool_id TEXT NOT NULL DEFAULT 'default';
ALTER TABLE servers ADD COLUMN kind TEXT NOT NULL DEFAULT 'standard';

CREATE INDEX IF NOT EXISTS idx_servers_pool ON servers (pool_id);

-- default 单例池:固定 id 与固定时间戳(单例系统行,时间无业务含义,避免双方言时间函数差异)。
INSERT OR IGNORE INTO server_pools (id, name, kind, cluster_id, created_at, updated_at)
VALUES ('default', 'default', 'standard', '', '1970-01-01T00:00:00Z', '1970-01-01T00:00:00Z');
