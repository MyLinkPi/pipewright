-- 0095 retention_config(MySQL):推送后回收本机镜像开关(语义同 sqlite 版)。
ALTER TABLE retention_config ADD COLUMN image_remove_after_push INT NOT NULL DEFAULT 1;
