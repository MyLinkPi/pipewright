-- 0049 service_reg(MySQL):服务注册网关(独立部署的 nginx 容器)。
-- 基域(泛域名证书 SealSecret 加密存 BLOB)+ 服务(abc → abc.efg.com,HTTP/TCP 反代)。
-- 证书 PEM 绝无明文入库;upload_token 只存 sha256。
CREATE TABLE IF NOT EXISTS service_reg_settings (
    id                VARCHAR(32) PRIMARY KEY,
    server_id         VARCHAR(64) NOT NULL DEFAULT '',
    http_port         INT NOT NULL DEFAULT 80,
    https_port        INT NOT NULL DEFAULT 443,
    image             VARCHAR(255) NOT NULL DEFAULT 'nginx:stable-alpine',
    network           VARCHAR(128) NOT NULL DEFAULT 'pipewright-gateway',
    container_name    VARCHAR(128) NOT NULL DEFAULT 'pipewright-nginx',
    volume_name       VARCHAR(128) NOT NULL DEFAULT 'pipewright_nginx',
    upload_token_hash VARCHAR(64) NOT NULL DEFAULT '',
    last_apply_at     VARCHAR(32) NOT NULL DEFAULT '',
    last_apply_error  VARCHAR(2048) NOT NULL DEFAULT '',
    created_at        VARCHAR(32) NOT NULL,
    updated_at        VARCHAR(32) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
CREATE TABLE IF NOT EXISTS service_reg_domains (
    id              VARCHAR(64) PRIMARY KEY,
    base_domain     VARCHAR(253) NOT NULL,
    cert_pem_sealed BLOB,
    key_pem_sealed  BLOB,
    cert_subject    VARCHAR(1024) NOT NULL DEFAULT '',
    cert_expires_at VARCHAR(32) NOT NULL DEFAULT '',
    created_at      VARCHAR(32) NOT NULL,
    updated_at      VARCHAR(32) NOT NULL,
    UNIQUE KEY uq_service_reg_domains_base (base_domain)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
CREATE TABLE IF NOT EXISTS service_reg_services (
    id              VARCHAR(64) PRIMARY KEY,
    domain_id       VARCHAR(64) NOT NULL,
    name            VARCHAR(63) NOT NULL,
    protocol        VARCHAR(8) NOT NULL DEFAULT 'http',
    upstream_kind   VARCHAR(16) NOT NULL DEFAULT 'container',
    upstream        VARCHAR(253) NOT NULL DEFAULT '',
    upstream_port   INT NOT NULL,
    tcp_listen_port INT NOT NULL DEFAULT 0,
    enabled         INT NOT NULL DEFAULT 1,
    created_at      VARCHAR(32) NOT NULL,
    updated_at      VARCHAR(32) NOT NULL,
    UNIQUE KEY uq_service_reg_services (domain_id, name),
    INDEX idx_service_reg_services_domain (domain_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
