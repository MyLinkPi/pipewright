-- 0102 cert_deploy_kinds(SQLite):证书部署目标类型化 —— 新增 kind('ssh' = 原有
-- SSH 落盘形态,默认,存量行自动归此; 'safeline' = 经雷池 WAF OpenAPI 推证书)。
--   waf_instance_id: kind=safeline 时引用安全中心 scanner_instances(id 逻辑引用,
--                     不设 FK;实例删除由应用层校验报人话错误)
--   cert_name      : kind=safeline 时雷池侧的证书名(同名原位更新,站点绑定不变)
-- server_id 语义不变(safeline 目标 = SafeLine 实例登记的部署宿主,后端自动填充)。

ALTER TABLE cert_deploys ADD COLUMN kind TEXT NOT NULL DEFAULT 'ssh';
ALTER TABLE cert_deploys ADD COLUMN waf_instance_id TEXT NOT NULL DEFAULT '';
ALTER TABLE cert_deploys ADD COLUMN cert_name TEXT NOT NULL DEFAULT '';
