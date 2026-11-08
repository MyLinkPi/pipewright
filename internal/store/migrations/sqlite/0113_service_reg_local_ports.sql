-- 0113 服务注册网关(SQLite):本机(__local__ 网关主机)共存模式回环端口
-- (specs/local-node-roles.md 7.3)。平台 HTTPS 占宿主 80/443 时,本机网关容器改绑
-- 127.0.0.1:<local 端口>,入口 vhost 反代进来。默认 18080/18443;校验(领域层):
-- 互不相等且不得与集群 http_port/https_port 相同。
ALTER TABLE service_reg_settings ADD COLUMN local_http_port INTEGER NOT NULL DEFAULT 18080;
ALTER TABLE service_reg_settings ADD COLUMN local_https_port INTEGER NOT NULL DEFAULT 18443;
