-- 0082 db_instances(SQLite):数据库管理——受管 MySQL/PostgreSQL 实例注册表。
--   password_sealed 为连接密码经 vault.SealSecret 的密文 BLOB(绝无明文落库/出响应);
--   readonly 默认 1:管理端所有写操作(行编辑、DDL、导入、用户管理、SQL 控制台
--   非查询语句)在领域层拒绝,解锁是显式且留审计的独立动作;
--   source=appstore 表示应用市场部署 mysql/postgres 模板后自动登记,
--   engine+host+port 唯一(重新部署同地址原地轮换密码,不产生重复条目)。

CREATE TABLE IF NOT EXISTS db_instances (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    engine          TEXT NOT NULL,
    host            TEXT NOT NULL,
    port            INTEGER NOT NULL,
    username        TEXT NOT NULL,
    password_sealed BLOB NOT NULL,
    tls             TEXT NOT NULL DEFAULT '',
    readonly        INTEGER NOT NULL DEFAULT 1,
    source          TEXT NOT NULL DEFAULT 'manual',
    server_id       TEXT NOT NULL DEFAULT '',
    stack_name      TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_db_instances_endpoint ON db_instances (engine, host, port);
