-- 0056 platform_https(MySQL):平台 HTTPS 访问(宿主 nginx 自动配置)。
-- 单行设置表;证书经 cert_id 软引用 certificates(PEM 绝不入本表)。
CREATE TABLE IF NOT EXISTS platform_https_settings (
    id              VARCHAR(32) PRIMARY KEY,
    enabled         INT NOT NULL DEFAULT 0,
    server_id       VARCHAR(64) NOT NULL DEFAULT '',
    domain          VARCHAR(253) NOT NULL DEFAULT '',
    cert_id         VARCHAR(64) NOT NULL DEFAULT '',
    upstream_host   VARCHAR(253) NOT NULL DEFAULT '127.0.0.1',
    upstream_port   INT NOT NULL DEFAULT 0,
    http_redirect   INT NOT NULL DEFAULT 1,
    status          VARCHAR(16) NOT NULL DEFAULT '',
    status_detail   VARCHAR(2048) NOT NULL DEFAULT '',
    last_applied_at VARCHAR(32) NOT NULL DEFAULT '',
    created_at      VARCHAR(32) NOT NULL,
    updated_at      VARCHAR(32) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
