-- 0049 service_reg:服务注册网关(独立部署的 nginx 容器)。
-- 模型:基域(efg.com,泛域名证书手动上传/脚本上传)+ 服务(abc → abc.efg.com,HTTP/TCP 反代)。
-- *.efg.com 由用户自行解析到网关主机 IP,平台不接管 DNS。
--
--   service_reg_settings  单例行(id='default'):网关主机 / 端口 / 镜像 / 网络 / 容器名 / 卷名
--                         + 证书上传 API token 的 sha256(仅哈希,明文只在生成时展示一次)。
--   service_reg_domains   基域;证书 PEM 经 vault SealSecret 加密后存 BLOB(绝无明文入库/回 API)。
--   service_reg_services  注册的服务;upstream 语义随 upstream_kind 而定
--                         (container=容器名,接入共享网络按名解析;address=host:port 任意地址)。
CREATE TABLE IF NOT EXISTS service_reg_settings (
    id                TEXT PRIMARY KEY,
    server_id         TEXT NOT NULL DEFAULT '',
    http_port         INTEGER NOT NULL DEFAULT 80,
    https_port        INTEGER NOT NULL DEFAULT 443,
    image             TEXT NOT NULL DEFAULT 'nginx:stable-alpine',
    network           TEXT NOT NULL DEFAULT 'pipewright-gateway',
    container_name    TEXT NOT NULL DEFAULT 'pipewright-nginx',
    volume_name       TEXT NOT NULL DEFAULT 'pipewright_nginx',
    upload_token_hash TEXT NOT NULL DEFAULT '',
    last_apply_at     TEXT NOT NULL DEFAULT '',
    last_apply_error  TEXT NOT NULL DEFAULT '',
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS service_reg_domains (
    id              TEXT PRIMARY KEY,
    base_domain     TEXT NOT NULL UNIQUE,
    cert_pem_sealed BLOB,
    key_pem_sealed  BLOB,
    cert_subject    TEXT NOT NULL DEFAULT '',
    cert_expires_at TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS service_reg_services (
    id              TEXT PRIMARY KEY,
    domain_id       TEXT NOT NULL,
    name            TEXT NOT NULL,
    protocol        TEXT NOT NULL DEFAULT 'http',
    upstream_kind   TEXT NOT NULL DEFAULT 'container',
    upstream        TEXT NOT NULL DEFAULT '',
    upstream_port   INTEGER NOT NULL,
    tcp_listen_port INTEGER NOT NULL DEFAULT 0,
    enabled         INTEGER NOT NULL DEFAULT 1,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    UNIQUE (domain_id, name)
);
CREATE INDEX IF NOT EXISTS idx_service_reg_services_domain ON service_reg_services (domain_id);
