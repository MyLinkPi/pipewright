-- 0062 registry_hub 新增 artifact_data_dir / cache_data_dir:制品与缓存 registry 的镜像存储
-- 目录独立可配(控制机本机绝对路径;空 = 回退默认 <BaseDir>/data 与 <BaseDir>/cache)。
-- 变更仅在显式「部署 / 更新」时收敛:制品目录变更 → 先停制品容器再把旧数据整体迁移到新目录
-- (旧数据不可再生,必须迁移);缓存目录变更 → 停缓存容器后直接清除旧目录(缓存可再生)。
ALTER TABLE registry_hub_config ADD COLUMN artifact_data_dir TEXT NOT NULL DEFAULT '';
ALTER TABLE registry_hub_config ADD COLUMN cache_data_dir TEXT NOT NULL DEFAULT '';
