-- 0104 dbaas(MySQL):数据库即服务(表语义同 SQLite 0104)。
-- TEXT 改 VARCHAR;密码密文 VARBINARY(vault.SealSecret 产出);error 用 TEXT。
CREATE TABLE IF NOT EXISTS dbaas_instances (
    id                     VARCHAR(64)  PRIMARY KEY,
    name                   VARCHAR(64)  NOT NULL,
    engine                 VARCHAR(16)  NOT NULL,
    version                VARCHAR(32)  NOT NULL,
    server_id              VARCHAR(64)  NOT NULL,
    host_port              INT          NOT NULL,
    admin_password_sealed  VARBINARY(512) NOT NULL DEFAULT '',
    stack_name             VARCHAR(128) NOT NULL,
    cpus                   VARCHAR(16)  NOT NULL DEFAULT '',
    memory                 VARCHAR(16)  NOT NULL DEFAULT '',
    status                 VARCHAR(16)  NOT NULL DEFAULT 'creating',
    status_detail          TEXT         NOT NULL,
    backup_interval_hours  INT          NOT NULL DEFAULT 0,
    backup_retention       INT          NOT NULL DEFAULT 7,
    last_backup_at         VARCHAR(32)  NOT NULL DEFAULT '',
    created_at             VARCHAR(32)  NOT NULL,
    updated_at             VARCHAR(32)  NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE UNIQUE INDEX idx_dbaas_instances_name ON dbaas_instances (name);
CREATE INDEX idx_dbaas_instances_server ON dbaas_instances (server_id);

CREATE TABLE IF NOT EXISTS dbaas_backups (
    id          VARCHAR(64)  PRIMARY KEY,
    instance_id VARCHAR(64)  NOT NULL,
    file_name   VARCHAR(255) NOT NULL,
    size_bytes  BIGINT       NOT NULL DEFAULT 0,
    sha256      VARCHAR(64)  NOT NULL DEFAULT '',
    kind        VARCHAR(16)  NOT NULL DEFAULT 'manual',
    status      VARCHAR(16)  NOT NULL DEFAULT 'running',
    error       TEXT         NOT NULL,
    object_key  VARCHAR(512) NOT NULL DEFAULT '',
    created_at  VARCHAR(32)  NOT NULL,
    finished_at VARCHAR(32)  NOT NULL DEFAULT '',
    CONSTRAINT fk_dbaas_backups_instance FOREIGN KEY (instance_id) REFERENCES dbaas_instances (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE INDEX idx_dbaas_backups_instance ON dbaas_backups (instance_id, created_at);
