-- 0056 platform_https:平台 HTTPS 访问(宿主 nginx 自动配置)。
-- 当 nginx 所在机器(通常是平台自身所在主机,登记为目标服务器)装有宿主 nginx 时,
-- 平台经 SSH 自动:下发证书 → 写 /etc/nginx/conf.d/pipewright-platform.conf(443 ssl 反代
-- 平台 Web 端口 + 80→443 跳转)→ nginx -t → reload。证书来自 internal/certmgmt(cert_id 软引用,
-- 证书本体/密文仍在 certificates 表,本表不存 PEM)。
--
--   platform_https_settings  单例行(id='default'):启用位 / nginx 所在服务器 / 访问域名 /
--                             证书引用 / 反代上游(默认 127.0.0.1:平台端口)/ 80 跳转开关
--                             + 最近一次应用结果(status ∈ ''|active|failed)。
CREATE TABLE IF NOT EXISTS platform_https_settings (
    id              TEXT PRIMARY KEY,
    enabled         INTEGER NOT NULL DEFAULT 0,
    server_id       TEXT NOT NULL DEFAULT '',
    domain          TEXT NOT NULL DEFAULT '',
    cert_id         TEXT NOT NULL DEFAULT '',
    upstream_host   TEXT NOT NULL DEFAULT '127.0.0.1',
    upstream_port   INTEGER NOT NULL DEFAULT 0,
    http_redirect   INTEGER NOT NULL DEFAULT 1,
    status          TEXT NOT NULL DEFAULT '',
    status_detail   TEXT NOT NULL DEFAULT '',
    last_applied_at TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
);
