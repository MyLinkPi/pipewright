-- 0083 redis_instances(SQLite):Redis 管理——受管 Redis 实例注册表。
--   password_sealed 为连接密码经 vault.SealSecret 的密文 BLOB(绝无明文落库/出响应);
--   可为空(内网无密码实例);
--   db_num 为默认逻辑库(0-15,SELECT 切换,页面可临时改库);
--   readonly 默认 1:管理端写操作(删键、改 TTL)在领域层拒绝,解锁是显式且留审计的独立动作;
--   source=appstore 表示应用市场部署 redis 模板后自动登记,
--   host+port 唯一(重新部署同地址原地轮换密码,不产生重复条目)。

CREATE TABLE IF NOT EXISTS redis_instances (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    host            TEXT NOT NULL,
    port            INTEGER NOT NULL,
    password_sealed BLOB,
    db_num          INTEGER NOT NULL DEFAULT 0,
    readonly        INTEGER NOT NULL DEFAULT 1,
    source          TEXT NOT NULL DEFAULT 'manual',
    server_id       TEXT NOT NULL DEFAULT '',
    stack_name      TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_redis_instances_endpoint ON redis_instances (host, port);
