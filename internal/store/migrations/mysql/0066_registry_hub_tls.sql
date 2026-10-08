-- 0066 registry_hub 新增 tls_cert_id(MySQL):语义同 sqlite 版 —— 内置 registry 栈挂载
-- 「证书管理」证书(空 = 明文 HTTP);部署时落盘 PEM 挂载进容器,daemon.json 随 TLS 切换。
ALTER TABLE registry_hub_config ADD COLUMN tls_cert_id VARCHAR(128) NOT NULL DEFAULT '';
