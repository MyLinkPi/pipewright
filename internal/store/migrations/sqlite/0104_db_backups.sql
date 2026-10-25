-- 0104 db_backups:已登记实例的备份恢复(功能长在 /databases 与 /redis 页)。
--   db_instances / redis_instances 各加 4 列(在现有实例对象上生长,不建新管理面):
--     backup_interval_hours  定时备份间隔小时(0=关;默认 0,升级不改变旧行为);
--     backup_retention       本实例保留份数(默认 7);
--     last_backup_at         上次定时触发时间(RFC3339;触发即推进,防重复);
--     auto_upload_provider_id 备份完成后自动上传的对象存储配置(空=不上传)。
--   db_backup_records:一次实例备份的运行记录。instance_kind+instance_id 多态引用
--   db_instances/redis_instances(不设外键——跨两张表;实例删除时由领域层连带清理);
--   status: running|completed|failed;kind: manual|scheduled|pre_restore;file_name
--   为 DataDir/db-backups/<instance_id>/ 下文件;object_key 非空=已上传对象存储。
ALTER TABLE db_instances ADD COLUMN backup_interval_hours INTEGER NOT NULL DEFAULT 0;
ALTER TABLE db_instances ADD COLUMN backup_retention INTEGER NOT NULL DEFAULT 7;
ALTER TABLE db_instances ADD COLUMN last_backup_at TEXT NOT NULL DEFAULT '';
ALTER TABLE db_instances ADD COLUMN auto_upload_provider_id TEXT NOT NULL DEFAULT '';

ALTER TABLE redis_instances ADD COLUMN backup_interval_hours INTEGER NOT NULL DEFAULT 0;
ALTER TABLE redis_instances ADD COLUMN backup_retention INTEGER NOT NULL DEFAULT 7;
ALTER TABLE redis_instances ADD COLUMN last_backup_at TEXT NOT NULL DEFAULT '';
ALTER TABLE redis_instances ADD COLUMN auto_upload_provider_id TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS db_backup_records (
    id           TEXT PRIMARY KEY,
    instance_kind TEXT NOT NULL,
    instance_id  TEXT NOT NULL,
    file_name    TEXT NOT NULL,
    size_bytes   INTEGER NOT NULL DEFAULT 0,
    sha256       TEXT NOT NULL DEFAULT '',
    kind         TEXT NOT NULL DEFAULT 'manual',
    status       TEXT NOT NULL DEFAULT 'running',
    error        TEXT NOT NULL DEFAULT '',
    object_key   TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    finished_at  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_db_backup_records_instance ON db_backup_records (instance_kind, instance_id, created_at);
