-- 0115 服务注册网关(SQLite):服务级对外协议(edge_https)。
-- 现状:基域有证书时全域 80→301→443 强制 https,「协议 HTTP」的服务实际由 HTTPS 提供。
-- edge_https 把这一决定下沉到服务粒度(协议下拉 HTTP/HTTPS/TCP 三选;HTTPS = 80 该 path 301
-- 到 443,HTTP = 80 直连)。回填保持存量行为零变化:证书域下的 http 服务全部标 1(它们今天
-- 实际就是 https 服务),无证书域保持 0;渲染层对「证书域全 https」输出与旧版逐字节一致。
ALTER TABLE service_reg_services ADD COLUMN edge_https INTEGER NOT NULL DEFAULT 0;

UPDATE service_reg_services SET edge_https = 1
 WHERE protocol = 'http'
   AND domain_id IN (SELECT id FROM service_reg_domains WHERE cert_pem_sealed IS NOT NULL);
