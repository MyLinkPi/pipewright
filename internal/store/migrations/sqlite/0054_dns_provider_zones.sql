-- 0054 dns_provider_zones:DNS 提供商改为「一账户多根区」+ 凭据 ID/Secret 分离。
--   1. 新表 dns_provider_zones:一个提供商可托管多个根区(一对多;现实中一个账户
--      常同时管理多个 zone)。旧 dns_providers.base_domain 每行一个根域,搬迁成 zone 行。
--   2. dns_providers 新增 api_id 列:ID 类凭据(DNSPod SecretId / 阿里云 AccessKeyId)
--      非机密,明文存储、可回显可编辑;不再与 Secret 拼成 "id,secret" 逗号字串。
--      vault 凭据(TypeDNSToken)此后只存 Secret 半段。
--   3. 删除 base_domain 列(根区语义移入 dns_provider_zones)。
-- 注意:破坏式切换——存量逗号格式旧凭据不迁移,需在编辑页重填 Secret。
CREATE TABLE IF NOT EXISTS dns_provider_zones (
    id          TEXT PRIMARY KEY,
    provider_id TEXT NOT NULL,
    base_domain TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    UNIQUE (provider_id, base_domain)
);
CREATE INDEX IF NOT EXISTS idx_dns_provider_zones_provider ON dns_provider_zones (provider_id);
-- 旧 base_domain 搬迁为该提供商的第一个 zone(幂等由 schema_migrations 跟踪保证)。
INSERT INTO dns_provider_zones (id, provider_id, base_domain, created_at)
SELECT lower(hex(randomblob(16))), id, base_domain, created_at FROM dns_providers;
ALTER TABLE dns_providers ADD COLUMN api_id TEXT NOT NULL DEFAULT '';
ALTER TABLE dns_providers DROP COLUMN base_domain;
