-- 0072 object_stores:对象存储集成层(流水线 OSS 上传节点配套)。
-- 每行 = 一条对象存储接入配置;本期 type 仅 aliyun_oss(阿里云 OSS),后续可扩展多云。
--
-- 凭据按机密性拆开存储:
--   - AccessKeyId 非机密 → access_key_id 明文列,可回显可编辑;
--   - AccessKeySecret 机密 → 走 vault 加密存储(credential_id 引用),绝不入库明文/日志/响应/错误体。
--   id            : uuid v4
--   type          : aliyun_oss(应用层校验枚举)
--   name          : 展示名(用户可读)
--   endpoint      : 服务接入点(如 oss-cn-hangzhou.aliyuncs.com)
--   bucket        : 默认存储桶
--   access_key_id : 非机密 AccessKeyId(明文可回显)
--   credential_id : 指向 credentials.id 的 vault 凭据引用(不设外键:删凭据由 vault 在用守卫拦)
CREATE TABLE IF NOT EXISTS object_stores (
    id            TEXT PRIMARY KEY,
    type          TEXT NOT NULL,
    name          TEXT NOT NULL,
    endpoint      TEXT NOT NULL,
    bucket        TEXT NOT NULL,
    access_key_id TEXT NOT NULL,
    credential_id TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_object_stores_type ON object_stores (type);
