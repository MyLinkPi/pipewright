-- 0081 service_reg_path_static(MySQL):path 子路径 + 静态托管,列语义见 sqlite 同号迁移。
ALTER TABLE service_reg_settings ADD COLUMN static_root VARCHAR(255) NOT NULL DEFAULT '/var/lib/pipewright-nginx/static';
ALTER TABLE service_reg_services
    ADD COLUMN path VARCHAR(128) NOT NULL DEFAULT '' AFTER name,
    ADD COLUMN strip_path_prefix TINYINT NOT NULL DEFAULT 0 AFTER tcp_listen_port;
ALTER TABLE service_reg_services DROP INDEX uq_service_reg_services;
ALTER TABLE service_reg_services ADD UNIQUE KEY uq_service_reg_services (domain_id, name, path);
