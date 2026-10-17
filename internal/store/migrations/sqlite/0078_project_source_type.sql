-- 0078 project_source_type(SQLite):项目源类型——git(默认)与 pushed(被推送的产物/镜像)。
-- pushed 项目由对等节点首次推送自动创建,不走手工建库(保证一一对应完整):
--   origin_node_id      推送来源节点 id(空 = 普通项目)
--   origin_project_ref  来源项目 id(与 origin_node_id 共同构成对应键,唯一性由服务层在
--                       SQLite 单写串行下以 check-then-insert 保证,不建 DB 唯一索引)
-- repo_url/credential_id 对 pushed 项目无意义(恒空),克隆校验对其跳过。

ALTER TABLE projects ADD COLUMN source_type TEXT NOT NULL DEFAULT 'git';
ALTER TABLE projects ADD COLUMN origin_node_id TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN origin_project_ref TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_projects_origin ON projects (origin_node_id);

-- pushed 项目的 credential_id 仍受 NOT NULL + FK(credentials) 约束:由项目领域在首次
-- CreatePushed 时惰性插入系统占位凭据行 '__pushed__'(ciphertext 空 BLOB,永不解密;
-- 被 pushed 项目引用时 RESTRICT 防误删,误删后领域层自愈补插)。不在迁移期插入 ——
-- 保持「全新安装 0 凭据」不变式,未用集群的部署永远看不到该行。
