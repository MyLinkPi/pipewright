-- 0072 object_stores(MySQL):对象存储集成层(流水线 OSS 上传节点配套)。
-- 每行 = 一条对象存储接入配置;本期 type 仅 aliyun_oss(阿里云 OSS),后续可扩展多云。
-- 凭据绝不以明文入此表:AccessKeyId 非机密,明文列;AccessKeySecret 只存 credential_id 引用
-- (指向 credentials 表里经 vault secretbox 加密的凭据),取明文仅在 apply 时经 vault.Reveal
-- 在进程内进行,绝不日志/回库/回 API。credential_id 不设外键。
-- type ∈ aliyun_oss。
CREATE TABLE IF NOT EXISTS object_stores (
    id            VARCHAR(64) PRIMARY KEY,
    type          VARCHAR(32) NOT NULL,
    name          VARCHAR(255) NOT NULL,
    endpoint      VARCHAR(255) NOT NULL,
    bucket        VARCHAR(255) NOT NULL,
    access_key_id VARCHAR(255) NOT NULL,
    credential_id VARCHAR(64) NOT NULL,
    created_at    VARCHAR(32) NOT NULL,
    updated_at    VARCHAR(32) NOT NULL,
    INDEX idx_object_stores_type (type)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
