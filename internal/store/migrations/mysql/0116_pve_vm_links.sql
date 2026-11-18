-- 0116 pve_vm_links(MySQL):PVE 建机后自动登记的「虚机 ↔ servers 行」映射。
-- 口径同 sqlite 孪生与 0089 aliyun_ecs_instance_links:按 (cluster_id, node, vmid)
-- 幂等匹配原机器行;servers 行删除 CASCADE 清理映射。
CREATE TABLE IF NOT EXISTS pve_vm_links (
    cluster_id VARCHAR(64) NOT NULL,
    node       VARCHAR(64) NOT NULL,
    vmid       BIGINT      NOT NULL,
    server_id  VARCHAR(64) NOT NULL,
    updated_at VARCHAR(32) NOT NULL,
    PRIMARY KEY (cluster_id, node, vmid),
    INDEX idx_pve_vm_links_server (server_id),
    CONSTRAINT fk_pve_vm_links_server FOREIGN KEY (server_id) REFERENCES servers (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;