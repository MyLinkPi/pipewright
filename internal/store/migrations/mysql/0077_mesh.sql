-- 0077 mesh(MySQL):集群——对等节点 + EasyTier 自组网(表语义同 SQLite 0077)。
-- TEXT 主键改 VARCHAR;私钥/网络密钥为 varbinary 密文 BLOB(vault.SealSecret 产出)。

CREATE TABLE IF NOT EXISTS mesh_settings (
    id             INT          PRIMARY KEY,
    node_id        VARCHAR(64)  NOT NULL,
    name           VARCHAR(255) NOT NULL DEFAULT '',
    public_key     VARCHAR(128) NOT NULL DEFAULT '',
    private_key    VARBINARY(512),
    enabled        INT          NOT NULL DEFAULT 0,
    api_port       INT          NOT NULL DEFAULT 0,
    easytier_mode  VARCHAR(16)  NOT NULL DEFAULT 'managed',
    easytier_rpc   VARCHAR(64)  NOT NULL DEFAULT '127.0.0.1:15888',
    network_name   VARCHAR(255) NOT NULL DEFAULT '',
    network_secret VARBINARY(512),
    virtual_ip     VARCHAR(64)  NOT NULL DEFAULT '',
    peers          TEXT         NOT NULL,
    created_at     VARCHAR(32)  NOT NULL,
    updated_at     VARCHAR(32)  NOT NULL,
    CONSTRAINT chk_mesh_settings_singleton CHECK (id = 1)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS mesh_peers (
    node_id    VARCHAR(64)  PRIMARY KEY,
    name       VARCHAR(255) NOT NULL DEFAULT '',
    virtual_ip VARCHAR(64)  NOT NULL DEFAULT '',
    api_port   INT          NOT NULL DEFAULT 0,
    public_key VARCHAR(128) NOT NULL DEFAULT '',
    version    VARCHAR(64)  NOT NULL DEFAULT '',
    source     VARCHAR(16)  NOT NULL DEFAULT 'auto',
    public_url VARCHAR(255) NOT NULL DEFAULT '',
    last_seen  VARCHAR(32)  NOT NULL DEFAULT '',
    created_at VARCHAR(32)  NOT NULL,
    updated_at VARCHAR(32)  NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE INDEX idx_mesh_peers_virtual_ip ON mesh_peers (virtual_ip);

CREATE TABLE IF NOT EXISTS mesh_acl (
    id              INT          PRIMARY KEY,
    visible_to      TEXT         NOT NULL,
    push_allowed    TEXT         NOT NULL,
    trigger_allowed TEXT         NOT NULL,
    updated_at      VARCHAR(32)  NOT NULL DEFAULT '',
    CONSTRAINT chk_mesh_acl_singleton CHECK (id = 1)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
