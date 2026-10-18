-- 0084 pve pools(MySQL):服务器池 + PVE 集群纳管(表语义同 SQLite 0084)。
-- TEXT 主键/带 DEFAULT 的新列改 VARCHAR;布尔沿用 TINYINT 0/1;密文 BLOB 同型。

CREATE TABLE IF NOT EXISTS server_pools (
    id         VARCHAR(64)  PRIMARY KEY,
    name       VARCHAR(255) NOT NULL,
    kind       VARCHAR(16)  NOT NULL DEFAULT 'standard',
    cluster_id VARCHAR(64)  NOT NULL DEFAULT '',
    created_at VARCHAR(32)  NOT NULL,
    updated_at VARCHAR(32)  NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE UNIQUE INDEX idx_server_pools_name ON server_pools (name);

CREATE TABLE IF NOT EXISTS pve_clusters (
    id               VARCHAR(64)  PRIMARY KEY,
    name             VARCHAR(255) NOT NULL,
    api_url          VARCHAR(512) NOT NULL DEFAULT '',
    token_name       VARCHAR(255) NOT NULL DEFAULT '',
    token_sealed     BLOB,
    tls_mode         VARCHAR(16)  NOT NULL DEFAULT 'insecure',
    tls_fingerprint  VARCHAR(128) NOT NULL DEFAULT '',
    state            VARCHAR(16)  NOT NULL DEFAULT 'created',
    readonly         TINYINT      NOT NULL DEFAULT 1,
    created_at       VARCHAR(32)  NOT NULL,
    updated_at       VARCHAR(32)  NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE UNIQUE INDEX idx_pve_clusters_name ON pve_clusters (name);

ALTER TABLE servers ADD COLUMN pool_id VARCHAR(64) NOT NULL DEFAULT 'default';
ALTER TABLE servers ADD COLUMN kind VARCHAR(16) NOT NULL DEFAULT 'standard';

CREATE INDEX idx_servers_pool ON servers (pool_id);

INSERT IGNORE INTO server_pools (id, name, kind, cluster_id, created_at, updated_at)
VALUES ('default', 'default', 'standard', '', '1970-01-01T00:00:00Z', '1970-01-01T00:00:00Z');
