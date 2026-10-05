-- 0064 service_reg_multi_gateway(MySQL):网关支持多台机器,列语义见 sqlite 同号迁移。
ALTER TABLE service_reg_settings ADD COLUMN server_ids LONGTEXT NOT NULL DEFAULT ('[]');
ALTER TABLE service_reg_settings ADD COLUMN last_apply_errors LONGTEXT NOT NULL DEFAULT ('{}');
