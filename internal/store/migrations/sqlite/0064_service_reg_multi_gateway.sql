-- 0064 service_reg_multi_gateway:服务注册网关支持多台机器(每台部署完全一致的网关)。
--
-- service_reg_settings 新增:
--   server_ids        : 全部网关主机引用 id 列表 JSON('[]' = 未配置)。
--                       读侧为空时回退 server_id 单值(存量升级零配置);写侧同步
--                       server_id = 列表第一台(旧二进制读新库仍工作,可安全降级)。
--   last_apply_errors : 最近一轮逐台收敛错误 JSON(serverId → 错误文案;仅失败项)。

ALTER TABLE service_reg_settings ADD COLUMN server_ids TEXT NOT NULL DEFAULT '[]';
ALTER TABLE service_reg_settings ADD COLUMN last_apply_errors TEXT NOT NULL DEFAULT '{}';
