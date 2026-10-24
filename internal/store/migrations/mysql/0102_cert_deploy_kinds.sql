-- 0102 cert_deploy_kinds(MySQL):表语义同 SQLite 0102(目标类型化:ssh | safeline)。

ALTER TABLE cert_deploys ADD COLUMN kind VARCHAR(16) NOT NULL DEFAULT 'ssh';
ALTER TABLE cert_deploys ADD COLUMN waf_instance_id VARCHAR(64) NOT NULL DEFAULT '';
ALTER TABLE cert_deploys ADD COLUMN cert_name VARCHAR(255) NOT NULL DEFAULT '';
