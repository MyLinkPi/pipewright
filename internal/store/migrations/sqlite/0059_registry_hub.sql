-- 0059 registry_hub:平台内置本地 Docker registry 单例配置(id=1)。
-- 控制机本机 docker 上自管一个双服务 registry 栈(registry:2 × 2):
--   - 制品 registry(artifact_port,默认 5000):构建产物 push 目标;部署机从这里 pull。
--     补齐「环境未绑定 ImageRegistry 时产物只有本地 tag、远程部署 pull 必败」的缺口。
--   - 缓存 registry(cache_port,默认 5001):pull-through 代理上游(upstream_url,默认
--     Docker Hub;可切镜像加速商)。切换上游只改此配置并重建该容器,生产机 daemon.json 零改动。
-- 各机 daemon.json 只需配一次(registry-mirrors/insecure-registries 指向本 registry 地址),
-- 由设置页手动勾选机器下发(备份原文件 + 自动重启 docker + 验证生效)。
--
--   external_addr     : 其他机器可达的控制机地址(host/IP,无 scheme;enabled 时必填)
--   upstream_url      : 缓存 registry 的上游 registry API 地址(http/https)
--   artifact_port     : 制品 registry 端口(1-65535,默认 5000)
--   cache_port        : 缓存 registry 端口(1-65535,默认 5001;两者不得相同)
--   keep_per_project  : 每个镜像仓库保留最近 N 个 tag(0=不限;含 latest 语义的 tag 不删 latest 本身)
--   max_age_days      : 删除创建时间早于 N 天的 tag(0=不限);与 keep 取并集删除(同 run retention)
--   enabled           : 总开关(默认关;关=构建/部署保持旧行为,不下发 daemon.json)
CREATE TABLE IF NOT EXISTS registry_hub_config (
    id               INTEGER PRIMARY KEY CHECK (id = 1),
    enabled          INTEGER NOT NULL DEFAULT 0,
    external_addr    TEXT NOT NULL DEFAULT '',
    upstream_url     TEXT NOT NULL DEFAULT 'https://registry-1.docker.io',
    artifact_port    INTEGER NOT NULL DEFAULT 5000,
    cache_port       INTEGER NOT NULL DEFAULT 5001,
    keep_per_project INTEGER NOT NULL DEFAULT 0,
    max_age_days     INTEGER NOT NULL DEFAULT 0,
    created_at       TEXT NOT NULL DEFAULT '',
    updated_at       TEXT NOT NULL DEFAULT ''
);
INSERT OR IGNORE INTO registry_hub_config
    (id, enabled, external_addr, upstream_url, artifact_port, cache_port, keep_per_project, max_age_days, created_at, updated_at)
VALUES (1, 0, '', 'https://registry-1.docker.io', 5000, 5001, 0, 0, '', '');
