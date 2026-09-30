-- 0051 service_reg_instances(MySQL):服务多实例(实例级轮转升级的地基)。
-- 存量迁移:既有 http+container 服务的单一 upstream 自动变为实例 #1。
CREATE TABLE IF NOT EXISTS service_reg_instances (
    id         VARCHAR(64) PRIMARY KEY,
    service_id VARCHAR(64) NOT NULL,
    container  VARCHAR(128) NOT NULL,
    port       INT NOT NULL DEFAULT 0,
    attached   INT NOT NULL DEFAULT 1,
    created_at VARCHAR(32) NOT NULL,
    updated_at VARCHAR(32) NOT NULL,
    UNIQUE KEY uq_service_reg_instances (service_id, container),
    INDEX idx_service_reg_instances_service (service_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
INSERT INTO service_reg_instances (id, service_id, container, port, attached, created_at, updated_at)
SELECT UUID(), s.id, s.upstream, s.upstream_port, 1, s.created_at, s.updated_at
FROM service_reg_services s
WHERE s.protocol = 'http' AND s.upstream_kind = 'container' AND s.upstream <> ''
  AND NOT EXISTS (SELECT 1 FROM service_reg_instances i WHERE i.service_id = s.id);
