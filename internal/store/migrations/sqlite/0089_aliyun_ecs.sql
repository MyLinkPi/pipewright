-- 0089 aliyun_ecs:阿里云 ECS 批量导入(设置→阿里云 ECS)。
-- aliyun_ecs_accounts 每行 = 一套阿里云账户接入配置(多账户并存;Region 是账户配置的一部分,
-- 多 Region 用多条配置表达)。凭据按机密性拆开存储(同 object_stores/dns_providers 先例):
--   - AccessKeyId 非机密 → access_key_id 明文列,可回显可编辑;
--   - AccessKeySecret 机密 → secret_sealed 密文 BLOB(vault.SealSecret 封存,同 pve_clusters.token_sealed),
--     绝不入库明文/日志/响应/错误体。
-- aliyun_ecs_instance_links 记录「ECS 实例 ↔ servers 行」的导入映射:再次导入时据此把实例
-- 更新到原机器行(实例 IP 变化后仍可正确匹配),而非按 IP 误建新行。servers 行删除后映射随
-- 之清理(CASCADE);账户删除由领域层显式连带清理(不设 account 外键,避免删除顺序耦合)。
CREATE TABLE IF NOT EXISTS aliyun_ecs_accounts (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    access_key_id TEXT NOT NULL,
    secret_sealed BLOB NOT NULL,
    region        TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS aliyun_ecs_instance_links (
    account_id  TEXT NOT NULL,
    instance_id TEXT NOT NULL,
    server_id   TEXT NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
    updated_at  TEXT NOT NULL,
    PRIMARY KEY (account_id, instance_id)
);
CREATE INDEX IF NOT EXISTS idx_aliyun_ecs_links_server ON aliyun_ecs_instance_links (server_id);
