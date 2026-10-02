-- 0062 registry_hub 新增 artifact_data_dir / cache_data_dir(MySQL):语义同 sqlite 版 ——
-- 制品/缓存存储目录独立可配,空 = 回退默认;变更经显式部署收敛(制品迁移旧数据,缓存清空旧目录)。
ALTER TABLE registry_hub_config ADD COLUMN artifact_data_dir VARCHAR(512) NOT NULL DEFAULT '';
ALTER TABLE registry_hub_config ADD COLUMN cache_data_dir VARCHAR(512) NOT NULL DEFAULT '';
