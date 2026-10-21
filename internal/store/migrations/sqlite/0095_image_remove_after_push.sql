-- 0095 retention_config(sqlite):推送后回收本机镜像开关。
--   image_remove_after_push(默认 1=回收):build_image 推送成功后立即删除构建机上的本机
--   tag(pipewright/<slug>:<commit>)——镜像已在 registry,构建机留存属磁盘浪费。关闭 = 留副本
--   复用构建层缓存(重建更快,代价是占盘)。与 0094 的磁盘治理列同源,独立于 enabled。
ALTER TABLE retention_config ADD COLUMN image_remove_after_push INTEGER NOT NULL DEFAULT 1;
