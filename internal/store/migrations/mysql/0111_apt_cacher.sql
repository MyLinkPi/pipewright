-- 0111 apt_cacher(MySQL):平台内置本地 apt 包缓存(apt-cacher-ng)单例配置(id=1)。
-- 语义同 sqlite 版:控制机自管 apt-cacher-ng 栈、代理片段手动勾选下发、可选 TLS 前端。
-- 详见 sqlite/0111_apt_cacher.sql 注释。惰性默认:不预插默认行。
CREATE TABLE IF NOT EXISTS apt_cacher_config (
    id            INT PRIMARY KEY CHECK (id = 1),
    enabled       TINYINT NOT NULL DEFAULT 0,
    external_addr VARCHAR(255) NOT NULL DEFAULT '',
    port          INT NOT NULL DEFAULT 3142,
    data_dir      VARCHAR(512) NOT NULL DEFAULT '',
    tls_cert_id   VARCHAR(128) NOT NULL DEFAULT '',
    created_at    VARCHAR(32) NOT NULL DEFAULT '',
    updated_at    VARCHAR(32) NOT NULL DEFAULT ''
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
