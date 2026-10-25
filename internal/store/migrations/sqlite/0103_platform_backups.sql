-- 0103 platform_backups(SQLite):平台自身数据备份恢复体系。
--   platform_backup_config:备份策略单例(id=1)。enabled 总开关(默认关);interval_hours
--     定时备份间隔小时(0=仅手动);retention_count 本地保留份数(0=不限,按最老先删);
--     include_artifacts 是否把制品库 artifacts/ 一并打包(默认关,量大);
--     include_master_key 是否把 vault master key 随包密封(默认关;开启时必须提供口令,
--     master key 经 argon2id(口令)→secretbox 密封为包内 master.key,绝不明文落包);
--     objectstore_provider_id 可选对象存储配置(非空 = 备份完成后自动上传异地容灾)。
--   platform_backups:每次备份的运行记录。status: running/completed/failed;
--     kind: manual/scheduled;sha256 为包文件校验和(下载/恢复前校验);object_key
--     非空表示已上传对象存储的 key(可重复上传覆盖)。
CREATE TABLE IF NOT EXISTS platform_backup_config (
    id                       INTEGER PRIMARY KEY CHECK (id = 1),
    enabled                  INTEGER NOT NULL DEFAULT 0,
    interval_hours           INTEGER NOT NULL DEFAULT 24,
    retention_count          INTEGER NOT NULL DEFAULT 7,
    include_artifacts        INTEGER NOT NULL DEFAULT 0,
    include_master_key       INTEGER NOT NULL DEFAULT 0,
    objectstore_provider_id  TEXT    NOT NULL DEFAULT '',
    last_run_at              TEXT    NOT NULL DEFAULT '',
    updated_at               TEXT    NOT NULL DEFAULT ''
);
INSERT OR IGNORE INTO platform_backup_config (id) VALUES (1);

CREATE TABLE IF NOT EXISTS platform_backups (
    id          TEXT PRIMARY KEY,
    file_name   TEXT NOT NULL,
    size_bytes  INTEGER NOT NULL DEFAULT 0,
    sha256      TEXT NOT NULL DEFAULT '',
    kind        TEXT NOT NULL DEFAULT 'manual',
    status      TEXT NOT NULL DEFAULT 'running',
    error       TEXT NOT NULL DEFAULT '',
    object_key  TEXT NOT NULL DEFAULT '',
    db_driver   TEXT NOT NULL DEFAULT 'sqlite',
    created_at  TEXT NOT NULL,
    finished_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_platform_backups_created ON platform_backups (created_at);
