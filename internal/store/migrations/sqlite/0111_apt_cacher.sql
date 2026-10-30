-- 0111 apt_cacher:平台内置本地 apt 包缓存(apt-cacher-ng)单例配置(id=1)。
-- 控制机本机 docker 上自管一个 apt-cacher-ng 栈(默认 3142),各 Debian 系机器的 apt
-- 经代理片段(/etc/apt/apt.conf.d/95pipewright-proxy)指向本缓存,由设置页手动勾选机器
-- 显式下发(备份原文件 + 原子替换 + apt-config dump 验证生效)。
--
--   external_addr : 其他机器可达的控制机地址(host/IP,无 scheme;enabled 时必填)
--   port          : 代理端口(1-65535,默认 3142)
--   data_dir      : 缓存存储目录(空 = /var/lib/pipewright/apt-cacher-ng/cache 默认)
--   tls_cert_id   : 「证书管理」证书 ID(空 = 明文 HTTP;非空 = nginx stream 前端 TLS 终止,
--                   apt 客户端严格校验,须公共可信且 SAN 覆盖 external_addr)
--   enabled       : 总开关(默认关;关 = 不部署栈、不下发代理片段)
-- 惰性默认语义与 registry_hub_config 一致:不预插默认行,无行时 Get 返回默认(关闭)。
CREATE TABLE IF NOT EXISTS apt_cacher_config (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    enabled       INTEGER NOT NULL DEFAULT 0,
    external_addr TEXT NOT NULL DEFAULT '',
    port          INTEGER NOT NULL DEFAULT 3142,
    data_dir      TEXT NOT NULL DEFAULT '',
    tls_cert_id   TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL DEFAULT '',
    updated_at    TEXT NOT NULL DEFAULT ''
);
