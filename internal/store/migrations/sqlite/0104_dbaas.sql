-- 0104 dbaas(SQLite):数据库即服务——实例生命周期 + 备份记录。
--   dbaas_instances:按需创建的 MySQL/PostgreSQL/Redis 实例。name 全局唯一(兼作
--     compose 栈名前缀 db-<name>);engine ∈ mysql|postgres|redis;host_port 为目标机
--     暴露端口(默认从 33000-40000 自动分配);admin_password_sealed 为管理员口令经
--     vault.SealSecret 的密文 BLOB(绝无明文落库/出响应);status 为行内生命周期状态
--     (creating|running|error|deleted),容器实时状态另经 docker ps 探测;
--     backup_interval_hours>0 表示启用定时备份,backup_retention 为本实例备份保留份数。
--   dbaas_backups:一次备份的运行记录(status: running|completed|failed;kind:
--     manual|scheduled|pre_restore);object_key 非空表示已上传对象存储。
CREATE TABLE IF NOT EXISTS dbaas_instances (
    id                     TEXT PRIMARY KEY,
    name                   TEXT NOT NULL,
    engine                 TEXT NOT NULL,
    version                TEXT NOT NULL,
    server_id              TEXT NOT NULL,
    host_port              INTEGER NOT NULL,
    admin_password_sealed  BLOB NOT NULL DEFAULT x'',
    stack_name             TEXT NOT NULL,
    cpus                   TEXT NOT NULL DEFAULT '',
    memory                 TEXT NOT NULL DEFAULT '',
    status                 TEXT NOT NULL DEFAULT 'creating',
    status_detail          TEXT NOT NULL DEFAULT '',
    backup_interval_hours  INTEGER NOT NULL DEFAULT 0,
    backup_retention       INTEGER NOT NULL DEFAULT 7,
    last_backup_at         TEXT NOT NULL DEFAULT '',
    created_at             TEXT NOT NULL,
    updated_at             TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_dbaas_instances_name ON dbaas_instances (name);
CREATE INDEX IF NOT EXISTS idx_dbaas_instances_server ON dbaas_instances (server_id);

CREATE TABLE IF NOT EXISTS dbaas_backups (
    id          TEXT PRIMARY KEY,
    instance_id TEXT NOT NULL REFERENCES dbaas_instances(id) ON DELETE CASCADE,
    file_name   TEXT NOT NULL,
    size_bytes  INTEGER NOT NULL DEFAULT 0,
    sha256      TEXT NOT NULL DEFAULT '',
    kind        TEXT NOT NULL DEFAULT 'manual',
    status      TEXT NOT NULL DEFAULT 'running',
    error       TEXT NOT NULL DEFAULT '',
    object_key  TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    finished_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_dbaas_backups_instance ON dbaas_backups (instance_id, created_at);
