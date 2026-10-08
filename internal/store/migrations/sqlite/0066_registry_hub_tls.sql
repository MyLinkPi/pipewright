-- 0066 registry_hub 新增 tls_cert_id:内置 registry 栈挂载 TLS 证书(引用「证书管理」
-- certmgmt 的证书 ID;空 = 明文 HTTP 现状)。非空时部署会在控制机 <BaseDir>/certs 落盘
-- PEM 并挂载进双服务容器(REGISTRY_HTTP_TLS_*),daemon.json 下发随之改为 https mirror
-- 且不再写 insecure-registries(要求证书公共可信,客户端零配置)。
ALTER TABLE registry_hub_config ADD COLUMN tls_cert_id TEXT NOT NULL DEFAULT '';
