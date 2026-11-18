-- 0116 pve_vm_links:PVE 建机后自动登记的「虚机 ↔ servers 行」映射。
-- 口径同 0089 aliyun_ecs_instance_links:再次建机/登记时据此把虚机更新到原机器行
-- (IP 变化后仍可正确匹配),而非按 IP 误建新行。servers 行删除后映射随之清理(CASCADE);
-- 集群删除由领域层显式连带清理(不设 cluster 外键,避免删除顺序耦合)。
CREATE TABLE IF NOT EXISTS pve_vm_links (
    cluster_id TEXT NOT NULL,
    node       TEXT NOT NULL,
    vmid       INTEGER NOT NULL,
    server_id  TEXT NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (cluster_id, node, vmid)
);
CREATE INDEX IF NOT EXISTS idx_pve_vm_links_server ON pve_vm_links (server_id);