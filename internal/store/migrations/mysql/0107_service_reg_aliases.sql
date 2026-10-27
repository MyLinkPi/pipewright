-- 0107 service_reg_aliases(MySQL):服务注册多子域名 —— 一个服务可绑定同基域下的多个
--   额外子域名(别名),nginx 同一 server 块 server_name 列出全部域名,任一别名都路由到
--   该服务。aliases 为逗号分隔的子域名 label 列表(serviceNameRe 字符集不含逗号,安全),
--   主路由键 name/(domain_id, name, path) 唯一约束不变;别名与全部既有域名(主名或别名)
--   的互斥由领域层校验(ErrFQDNTaken)。

ALTER TABLE service_reg_services ADD COLUMN aliases TEXT NOT NULL DEFAULT ('');
