-- 0054 dns_provider_zones(MySQL):DNS 提供商改为「一账户多根区」+ 凭据 ID/Secret 分离。
--   1. 新表 dns_provider_zones:一个提供商可托管多个根区(一对多)。
--   2. dns_providers 新增 api_id:ID 类凭据(DNSPod SecretId / 阿里云 AccessKeyId)非机密,
--      明文存储、可回显可编辑;vault 凭据(TypeDNSToken)此后只存 Secret 半段。
--   3. 删除 base_domain 列(根区语义移入 dns_provider_zones)。
-- 注意:破坏式切换——存量逗号格式旧凭据不迁移,需在编辑页重填 Secret。
CREATE TABLE IF NOT EXISTS dns_provider_zones (
    id          VARCHAR(64) PRIMARY KEY,
    provider_id VARCHAR(64) NOT NULL,
    base_domain VARCHAR(253) NOT NULL,
    created_at  VARCHAR(32) NOT NULL,
    UNIQUE KEY uq_dns_provider_zones_domain (provider_id, base_domain),
    INDEX idx_dns_provider_zones_provider (provider_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
-- 旧 base_domain 搬迁为该提供商的第一个 zone。
INSERT INTO dns_provider_zones (id, provider_id, base_domain, created_at)
SELECT UUID(), id, base_domain, created_at FROM dns_providers;
ALTER TABLE dns_providers ADD COLUMN api_id VARCHAR(256) NOT NULL DEFAULT '';
ALTER TABLE dns_providers DROP COLUMN base_domain;
