-- 0068 service_reg_cluster_instances:实例集群化 —— 实例归属服务器(server_id) + 网关反代宿主端口(host_port)。
-- 模型从「网关本机容器 + 共享网络」迁移为「集群任意服务器的 服务器地址:宿主端口」:
--   容器实例  : server_id=部署目标机, container=容器名, host_port=部署时自动分配的宿主端口(0=待补齐);
--   非容器实例: container='', host_port=进程监听端口。
-- 唯一约束 (service_id, container) → (service_id, server_id, container)。存量实例 server_id=''
-- (渲染时跳过并提示待迁移,下次部署按容器名匹配 upsert 补齐)。SQLite 改约束需重建表。
CREATE TABLE service_reg_instances_new (
    id         TEXT PRIMARY KEY,
    service_id TEXT NOT NULL,
    server_id  TEXT NOT NULL DEFAULT '',
    container  TEXT NOT NULL DEFAULT '',
    port       INTEGER NOT NULL DEFAULT 0,
    host_port  INTEGER NOT NULL DEFAULT 0,
    attached   INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (service_id, server_id, container)
);
INSERT INTO service_reg_instances_new (id, service_id, server_id, container, port, host_port, attached, created_at, updated_at)
SELECT id, service_id, '', container, port, 0, attached, created_at, updated_at FROM service_reg_instances;
DROP TABLE service_reg_instances;
ALTER TABLE service_reg_instances_new RENAME TO service_reg_instances;
CREATE INDEX IF NOT EXISTS idx_service_reg_instances_service ON service_reg_instances (service_id);
CREATE INDEX IF NOT EXISTS idx_service_reg_instances_server ON service_reg_instances (server_id);
