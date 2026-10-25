-- 0103 platform_backups(MySQL):平台自身数据备份恢复体系(表语义同 SQLite 0103)。
-- TEXT 改 VARCHAR;备份运行记录的 error 用 TEXT(可能较长)。
CREATE TABLE IF NOT EXISTS platform_backup_config (
    id                       INT          PRIMARY KEY CHECK (id = 1),
    enabled                  INT          NOT NULL DEFAULT 0,
    interval_hours           INT          NOT NULL DEFAULT 24,
    retention_count          INT          NOT NULL DEFAULT 7,
    include_artifacts        INT          NOT NULL DEFAULT 0,
    include_master_key       INT          NOT NULL DEFAULT 0,
    objectstore_provider_id  VARCHAR(64)  NOT NULL DEFAULT '',
    last_run_at              VARCHAR(32)  NOT NULL DEFAULT '',
    updated_at               VARCHAR(32)  NOT NULL DEFAULT ''
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
INSERT IGNORE INTO platform_backup_config (id) VALUES (1);

CREATE TABLE IF NOT EXISTS platform_backups (
    id          VARCHAR(64)  PRIMARY KEY,
    file_name   VARCHAR(255) NOT NULL,
    size_bytes  BIGINT       NOT NULL DEFAULT 0,
    sha256      VARCHAR(64)  NOT NULL DEFAULT '',
    kind        VARCHAR(16)  NOT NULL DEFAULT 'manual',
    status      VARCHAR(16)  NOT NULL DEFAULT 'running',
    error       TEXT         NOT NULL,
    object_key  VARCHAR(512) NOT NULL DEFAULT '',
    db_driver   VARCHAR(16)  NOT NULL DEFAULT 'sqlite',
    created_at  VARCHAR(32)  NOT NULL,
    finished_at VARCHAR(32)  NOT NULL DEFAULT ''
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE INDEX idx_platform_backups_created ON platform_backups (created_at);
