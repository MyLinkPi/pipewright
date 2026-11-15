-- 0115 服务注册网关(MySQL):结构同 SQLite 版(服务级对外协议 edge_https + 行为零变化回填)。
ALTER TABLE service_reg_services ADD COLUMN edge_https TINYINT NOT NULL DEFAULT 0;

UPDATE service_reg_services SET edge_https = 1
 WHERE protocol = 'http'
   AND domain_id IN (SELECT id FROM service_reg_domains WHERE cert_pem_sealed IS NOT NULL);
