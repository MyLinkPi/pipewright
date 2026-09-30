-- 0051 service_reg_instances:服务多实例(实例级轮转升级的地基)。
-- 一个 http+container 服务的 upstream 由 N 个实例组成;实例 = 网关同机一个具名容器。
-- attached=0 表示已摘除(不渲染进 upstream、不接网络);容器重建式升级经 SwapInstance
-- 原子替换(单次 reload),配合 deploy 的 instance_rolling 策略实现零停机轮转。
-- 存量迁移:既有 http+container 服务的单一 upstream 自动变为实例 #1(行为不变)。
CREATE TABLE IF NOT EXISTS service_reg_instances (
    id         TEXT PRIMARY KEY,
    service_id TEXT NOT NULL,
    container  TEXT NOT NULL,
    port       INTEGER NOT NULL DEFAULT 0,
    attached   INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (service_id, container)
);
CREATE INDEX IF NOT EXISTS idx_service_reg_instances_service ON service_reg_instances (service_id);
INSERT INTO service_reg_instances (id, service_id, container, port, attached, created_at, updated_at)
SELECT lower(hex(randomblob(16))), s.id, s.upstream, s.upstream_port, 1, s.created_at, s.updated_at
FROM service_reg_services s
WHERE s.protocol = 'http' AND s.upstream_kind = 'container' AND s.upstream <> ''
  AND NOT EXISTS (SELECT 1 FROM service_reg_instances i WHERE i.service_id = s.id);
