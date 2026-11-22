-- 0118 pve_upgrade_jobs 加 host_node 列:PVE 控制端(Pipewright)自托管场景下,
-- 宿主节点由用户在向导中显式指定(不做自动识别,避免误判),排到最后升级、
-- dist-upgrade 完成后暂停等人工确认重启。空串 = 未指定(全自动滚动)。
ALTER TABLE pve_upgrade_jobs ADD COLUMN host_node TEXT NOT NULL DEFAULT '';