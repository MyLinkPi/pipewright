-- 0068 service_reg_cluster_instances(MySQL):实例集群化 —— server_id 归属 + host_port 宿主端口。
-- 存量实例 server_id=''(渲染跳过待迁移);唯一键 (service_id, container) → (service_id, server_id, container)。
ALTER TABLE service_reg_instances
    ADD COLUMN server_id VARCHAR(64) NOT NULL DEFAULT '' AFTER service_id,
    ADD COLUMN host_port INT NOT NULL DEFAULT 0 AFTER port,
    MODIFY COLUMN container VARCHAR(128) NOT NULL DEFAULT '';
ALTER TABLE service_reg_instances DROP INDEX uq_service_reg_instances;
ALTER TABLE service_reg_instances ADD UNIQUE KEY uq_service_reg_instances (service_id, server_id, container);
CREATE INDEX idx_service_reg_instances_server ON service_reg_instances (server_id);
