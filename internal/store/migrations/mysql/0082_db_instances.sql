-- 0082 db_instances(MySQL):数据库管理——受管 MySQL/PostgreSQL 实例注册表(表语义同 SQLite 0082)。
-- TEXT 主键改 VARCHAR;password_sealed 为 varbinary 密文 BLOB(vault.SealSecret 产出)。

CREATE TABLE IF NOT EXISTS db_instances (
    id              VARCHAR(64)  PRIMARY KEY,
    name            VARCHAR(255) NOT NULL,
    engine          VARCHAR(16)  NOT NULL,
    host            VARCHAR(255) NOT NULL,
    port            INT          NOT NULL,
    username        VARCHAR(255) NOT NULL,
    password_sealed VARBINARY(512) NOT NULL,
    tls             VARCHAR(16)  NOT NULL DEFAULT '',
    readonly        INT          NOT NULL DEFAULT 1,
    source          VARCHAR(16)  NOT NULL DEFAULT 'manual',
    server_id       VARCHAR(64)  NOT NULL DEFAULT '',
    stack_name      VARCHAR(128) NOT NULL DEFAULT '',
    created_at      VARCHAR(32)  NOT NULL,
    updated_at      VARCHAR(32)  NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE UNIQUE INDEX idx_db_instances_endpoint ON db_instances (engine, host, port);
