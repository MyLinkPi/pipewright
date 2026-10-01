-- 0059 registry_hub(MySQL):平台内置本地 Docker registry 单例配置(id=1)。
-- 语义同 sqlite 版:双服务 registry 栈(制品+pull-through 缓存)、daemon.json 手动下发、
-- 镜像 tag 自动保留策略。详见 sqlite/0059 注释。
CREATE TABLE IF NOT EXISTS registry_hub_config (
    id               INT PRIMARY KEY CHECK (id = 1),
    enabled          TINYINT NOT NULL DEFAULT 0,
    external_addr    VARCHAR(255) NOT NULL DEFAULT '',
    upstream_url     VARCHAR(512) NOT NULL DEFAULT 'https://registry-1.docker.io',
    artifact_port    INT NOT NULL DEFAULT 5000,
    cache_port       INT NOT NULL DEFAULT 5001,
    keep_per_project INT NOT NULL DEFAULT 0,
    max_age_days     INT NOT NULL DEFAULT 0,
    created_at       VARCHAR(32) NOT NULL DEFAULT '',
    updated_at       VARCHAR(32) NOT NULL DEFAULT ''
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
INSERT IGNORE INTO registry_hub_config
    (id, enabled, external_addr, upstream_url, artifact_port, cache_port, keep_per_project, max_age_days, created_at, updated_at)
VALUES (1, 0, '', 'https://registry-1.docker.io', 5000, 5001, 0, 0, '', '');
