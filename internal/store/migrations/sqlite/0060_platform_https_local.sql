-- 0060 platform_https 本机化:平台 HTTPS 固定作用于平台自身所在主机(本机执行,
-- 不再经 SSH 选择已登记服务器),server_id 列随之移除。
ALTER TABLE platform_https_settings DROP COLUMN server_id;
