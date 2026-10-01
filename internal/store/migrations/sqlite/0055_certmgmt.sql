-- 0055 certmgmt:证书管理(acme.sh 自动签发 / 手动导入)。
-- 证书是独立实体(不再挂于某个基域):一张证书可覆盖任意 SAN 集(含泛域名),
-- 签发/续期由平台集成的 acme.sh(网关主机上的 pipewright-acme 容器)完成,平台后台调度续期。
--
--   certificates  一张证书一行。PEM 经 vault SealSecret 加密存 BLOB(绝无明文入库/回 API);
--                 source=acme(平台签发,可续期)| manual(导入,只读展示+下发)。
--                 primary_domain 即 acme.sh 的证书主键(-d 第一域);domains 为逗号分隔全量 SAN。
--                 not_before/not_after/subject/issuer 从叶子证书解析;status ∈ pending|issued|failed。
--                 last_attempt_at 供 Sweeper 失败退避(距上次尝试 >24h 才重试)。
--                 dns_provider_id 引用 dns_providers(DNS-01 挑战凭据;manual 来源为空)。
CREATE TABLE IF NOT EXISTS certificates (
    id               TEXT PRIMARY KEY,
    primary_domain   TEXT NOT NULL,
    domains          TEXT NOT NULL DEFAULT '',
    source           TEXT NOT NULL DEFAULT 'acme',
    ca               TEXT NOT NULL DEFAULT '',
    validation       TEXT NOT NULL DEFAULT 'dns',
    dns_provider_id  TEXT NOT NULL DEFAULT '',
    key_type         TEXT NOT NULL DEFAULT 'ec-256',
    auto_renew       INTEGER NOT NULL DEFAULT 1,
    status           TEXT NOT NULL DEFAULT 'pending',
    status_detail    TEXT NOT NULL DEFAULT '',
    cert_pem_sealed  BLOB,
    key_pem_sealed   BLOB,
    subject          TEXT NOT NULL DEFAULT '',
    issuer           TEXT NOT NULL DEFAULT '',
    not_before       TEXT NOT NULL DEFAULT '',
    not_after        TEXT NOT NULL DEFAULT '',
    last_issued_at   TEXT NOT NULL DEFAULT '',
    last_attempt_at  TEXT NOT NULL DEFAULT '',
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_certificates_primary_domain ON certificates (primary_domain);
