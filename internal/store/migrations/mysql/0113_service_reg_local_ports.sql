-- 0113 服务注册网关(MySQL):结构同 SQLite 版(specs/local-node-roles.md 7.3)。
ALTER TABLE service_reg_settings ADD COLUMN local_http_port BIGINT NOT NULL DEFAULT 18080;
ALTER TABLE service_reg_settings ADD COLUMN local_https_port BIGINT NOT NULL DEFAULT 18443;
