-- 0082 ceph(MySQL):Ceph 集群部署与管理(表语义同 SQLite 0082)。
-- TEXT 主键改 VARCHAR;时间戳沿用 VARCHAR(32) RFC3339 与 mesh 先例一致。

CREATE TABLE IF NOT EXISTS ceph_clusters (
    id                      VARCHAR(64)  PRIMARY KEY,
    name                    VARCHAR(255) NOT NULL UNIQUE,
    fsid                    VARCHAR(64)  NOT NULL DEFAULT '',
    bootstrap_server_id     VARCHAR(64)  NOT NULL,
    image                   VARCHAR(255) NOT NULL DEFAULT '',
    cephadm_source          VARCHAR(255) NOT NULL DEFAULT 'auto',
    public_network          VARCHAR(64)  NOT NULL DEFAULT '',
    cluster_network         VARCHAR(64)  NOT NULL DEFAULT '',
    state                   VARCHAR(16)  NOT NULL DEFAULT 'created',
    dashboard_url           VARCHAR(255) NOT NULL DEFAULT '',
    dashboard_user          VARCHAR(64)  NOT NULL DEFAULT '',
    dashboard_credential_id VARCHAR(64)  NOT NULL DEFAULT '',
    created_at              VARCHAR(32)  NOT NULL,
    updated_at              VARCHAR(32)  NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS ceph_hosts (
    cluster_id VARCHAR(64)  NOT NULL,
    server_id  VARCHAR(64)  NOT NULL,
    roles      VARCHAR(255) NOT NULL DEFAULT '',
    labels     VARCHAR(255) NOT NULL DEFAULT '',
    devices    TEXT         NOT NULL,
    hostname   VARCHAR(255) NOT NULL DEFAULT '',
    addr       VARCHAR(255) NOT NULL DEFAULT '',
    added_at   VARCHAR(32)  NOT NULL,
    PRIMARY KEY (cluster_id, server_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE INDEX idx_ceph_hosts_cluster ON ceph_hosts (cluster_id);

CREATE TABLE IF NOT EXISTS ceph_osds (
    cluster_id VARCHAR(64) NOT NULL,
    host       VARCHAR(255) NOT NULL,
    device     VARCHAR(255) NOT NULL,
    osd_id     INT         NOT NULL DEFAULT -1,
    state      VARCHAR(16) NOT NULL DEFAULT 'pending',
    created_at VARCHAR(32) NOT NULL,
    PRIMARY KEY (cluster_id, host, device)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE INDEX idx_ceph_osds_cluster ON ceph_osds (cluster_id);
