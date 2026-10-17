-- 0081 service_reg_path_static:服务注册支持子路径(path)与纯静态托管(upstream_kind='static')。
--
-- service_reg_services 新增:
--   path             : URL 子路径('' = 根路径 /)。同一 FQDN(= 同 domain+name)下不同 path
--                      的服务共存,合并进同一 server 块的多 location;唯一约束相应放宽为
--                      (domain_id, name, path)(SQLite 改约束需重建表)。path 完全相同(含同为
--                      根路径)仍互斥,由领域层校验(ErrPathTaken)+ 唯一约束兜底。
--   strip_path_prefix: 反代是否剥离 location 前缀再转发(0=保留前缀原样透传,默认)。
--   upstream_kind='static': 网关 nginx 直接托管静态文件(dist),Upstream 恒空、
--                      upstream_port 恒 0(列放宽默认 0,存量行不受影响)。
-- service_reg_settings 新增:
--   static_root      : 网关主机上的静态发布根目录(bind mount 进网关容器的 /etc/pipewright/static)。
CREATE TABLE service_reg_services_new (
    id                TEXT PRIMARY KEY,
    domain_id         TEXT NOT NULL,
    name              TEXT NOT NULL,
    path              TEXT NOT NULL DEFAULT '',
    protocol          TEXT NOT NULL DEFAULT 'http',
    upstream_kind     TEXT NOT NULL DEFAULT 'container',
    upstream          TEXT NOT NULL DEFAULT '',
    upstream_port     INTEGER NOT NULL DEFAULT 0,
    tcp_listen_port   INTEGER NOT NULL DEFAULT 0,
    strip_path_prefix INTEGER NOT NULL DEFAULT 0,
    enabled           INTEGER NOT NULL DEFAULT 1,
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL,
    UNIQUE (domain_id, name, path)
);
INSERT INTO service_reg_services_new
    (id, domain_id, name, path, protocol, upstream_kind, upstream, upstream_port,
     tcp_listen_port, strip_path_prefix, enabled, created_at, updated_at)
SELECT id, domain_id, name, '', protocol, upstream_kind, upstream, upstream_port,
       tcp_listen_port, 0, enabled, created_at, updated_at
  FROM service_reg_services;
DROP TABLE service_reg_services;
ALTER TABLE service_reg_services_new RENAME TO service_reg_services;
CREATE INDEX IF NOT EXISTS idx_service_reg_services_domain ON service_reg_services (domain_id);

ALTER TABLE service_reg_settings ADD COLUMN static_root TEXT NOT NULL DEFAULT '/var/lib/pipewright-nginx/static';
