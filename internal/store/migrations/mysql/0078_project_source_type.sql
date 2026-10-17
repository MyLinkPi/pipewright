-- 0078 project_source_type(MySQL):项目源类型 git(默认)/ pushed(被推送的产物/镜像)。
-- 语义同 SQLite 0078;对应键唯一性由服务层保证(MySQL 无部分索引,不建 DB 唯一索引)。

ALTER TABLE projects ADD COLUMN source_type VARCHAR(16) NOT NULL DEFAULT 'git';
ALTER TABLE projects ADD COLUMN origin_node_id VARCHAR(64) NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN origin_project_ref VARCHAR(64) NOT NULL DEFAULT '';

CREATE INDEX idx_projects_origin ON projects (origin_node_id);

-- pushed 项目的 credential_id 仍受 NOT NULL + FK(credentials) 约束:由项目领域在首次
-- CreatePushed 时惰性插入系统占位凭据行 '__pushed__'(同 SQLite 0078 注释;保持全新安装 0 凭据)。
