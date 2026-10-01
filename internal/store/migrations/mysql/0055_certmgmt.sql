-- 0055 certmgmt(MySQL):证书管理(acme.sh 自动签发 / 手动导入)。
-- 证书是独立实体;PEM 经 vault SealSecret 加密存 BLOB(绝无明文入库/回 API)。
-- source=acme(平台签发,可续期)| manual(导入);status ∈ pending|issued|failed。
CREATE TABLE IF NOT EXISTS certificates (
    id               VARCHAR(64) PRIMARY KEY,
    primary_domain   VARCHAR(253) NOT NULL,
    domains          VARCHAR(2048) NOT NULL DEFAULT '',
    source           VARCHAR(8) NOT NULL DEFAULT 'acme',
    ca               VARCHAR(32) NOT NULL DEFAULT '',
    validation       VARCHAR(8) NOT NULL DEFAULT 'dns',
    dns_provider_id  VARCHAR(64) NOT NULL DEFAULT '',
    key_type         VARCHAR(16) NOT NULL DEFAULT 'ec-256',
    auto_renew       INT NOT NULL DEFAULT 1,
    status           VARCHAR(16) NOT NULL DEFAULT 'pending',
    status_detail    VARCHAR(2048) NOT NULL DEFAULT '',
    cert_pem_sealed  BLOB,
    key_pem_sealed   BLOB,
    subject          VARCHAR(1024) NOT NULL DEFAULT '',
    issuer           VARCHAR(1024) NOT NULL DEFAULT '',
    not_before       VARCHAR(32) NOT NULL DEFAULT '',
    not_after        VARCHAR(32) NOT NULL DEFAULT '',
    last_issued_at   VARCHAR(32) NOT NULL DEFAULT '',
    last_attempt_at  VARCHAR(32) NOT NULL DEFAULT '',
    created_at       VARCHAR(32) NOT NULL,
    updated_at       VARCHAR(32) NOT NULL,
    UNIQUE KEY uq_certificates_primary_domain (primary_domain)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
