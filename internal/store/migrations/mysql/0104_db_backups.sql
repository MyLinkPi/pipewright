-- 0104 db_backups(MySQL):已登记实例的备份恢复(表语义同 SQLite 0104)。
-- ALTER 无 IF NOT EXISTS,幂等由 schema_migrations 一次性保证。
ALTER TABLE db_instances ADD COLUMN backup_interval_hours INT NOT NULL DEFAULT 0;
ALTER TABLE db_instances ADD COLUMN backup_retention INT NOT NULL DEFAULT 7;
ALTER TABLE db_instances ADD COLUMN last_backup_at VARCHAR(32) NOT NULL DEFAULT '';
ALTER TABLE db_instances ADD COLUMN auto_upload_provider_id VARCHAR(64) NOT NULL DEFAULT '';

ALTER TABLE redis_instances ADD COLUMN backup_interval_hours INT NOT NULL DEFAULT 0;
ALTER TABLE redis_instances ADD COLUMN backup_retention INT NOT NULL DEFAULT 7;
ALTER TABLE redis_instances ADD COLUMN last_backup_at VARCHAR(32) NOT NULL DEFAULT '';
ALTER TABLE redis_instances ADD COLUMN auto_upload_provider_id VARCHAR(64) NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS db_backup_records (
    id            VARCHAR(64)  PRIMARY KEY,
    instance_kind VARCHAR(16)  NOT NULL,
    instance_id   VARCHAR(64)  NOT NULL,
    file_name     VARCHAR(255) NOT NULL,
    size_bytes    BIGINT       NOT NULL DEFAULT 0,
    sha256        VARCHAR(64)  NOT NULL DEFAULT '',
    kind          VARCHAR(16)  NOT NULL DEFAULT 'manual',
    status        VARCHAR(16)  NOT NULL DEFAULT 'running',
    error         TEXT         NOT NULL,
    object_key    VARCHAR(512) NOT NULL DEFAULT '',
    created_at    VARCHAR(32)  NOT NULL,
    finished_at   VARCHAR(32)  NOT NULL DEFAULT ''
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE INDEX idx_db_backup_records_instance ON db_backup_records (instance_kind, instance_id, created_at);
