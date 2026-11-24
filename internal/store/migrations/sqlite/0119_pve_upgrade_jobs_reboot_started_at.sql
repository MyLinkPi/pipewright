-- 0119 pve_upgrade_jobs 加 reboot_started_at 列:记录宿主节点人工确认重启的发出时间
-- (RFC3339)。续跑时用节点 uptime 与「确认重启后经过时长」比较,确认节点确实重启过,
-- 防止在控制端尚未下线时就点「续跑」导致误判完成。
ALTER TABLE pve_upgrade_jobs ADD COLUMN reboot_started_at TEXT NOT NULL DEFAULT '';
