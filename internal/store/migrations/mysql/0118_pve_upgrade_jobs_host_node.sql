-- 0118 pve_upgrade_jobs 加 host_node 列(MySQL):口径同 sqlite 孪生——宿主节点
-- 用户显式指定,最后升级、重启需人工确认;空串 = 未指定。
ALTER TABLE pve_upgrade_jobs ADD COLUMN host_node VARCHAR(64) NOT NULL DEFAULT '';