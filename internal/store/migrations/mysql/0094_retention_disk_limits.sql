-- 0094 retention_config(MySQL):磁盘治理配置列(挂在保留策略单例上)。
--   cache_max_gb    构建依赖缓存总量上限(GB,LRU 按 mtime 淘汰;0=不限;默认 10);
--   image_keep_tags 每项目保留的本地镜像 tag 数(pipewright/<slug>:<commit> 更旧的轮转删除;
--                   0=不轮转;默认 10)。
-- 两项独立于 enabled 总开关——缓存/镜像不是审计数据,默认即生效回收;run 数据裁剪仍由
-- enabled 控制(升级不改变旧行为)。
ALTER TABLE retention_config ADD COLUMN cache_max_gb INT NOT NULL DEFAULT 10;
ALTER TABLE retention_config ADD COLUMN image_keep_tags INT NOT NULL DEFAULT 10;
