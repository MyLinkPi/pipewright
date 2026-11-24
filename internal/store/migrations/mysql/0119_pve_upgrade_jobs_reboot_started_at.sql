-- 0119 pve_upgrade_jobs 加 reboot_started_at 列(MySQL):口径同 sqlite 孪生。
ALTER TABLE pve_upgrade_jobs ADD COLUMN reboot_started_at VARCHAR(32) NOT NULL DEFAULT '';
