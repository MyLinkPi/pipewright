-- 0061 system_config 新增 release_mirror(MySQL):自升级镜像源 base URL。
-- 空 = 回退 GitHub 官方源;非空须为 GitHub 路径兼容的镜像 base,修改即时生效。
ALTER TABLE system_config ADD COLUMN release_mirror VARCHAR(512) NOT NULL DEFAULT '';
