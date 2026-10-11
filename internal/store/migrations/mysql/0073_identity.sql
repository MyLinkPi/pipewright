-- 0073 identity(MySQL):普通用户体系。
--   users            —— 普通用户(member)账号;管理员仍存于 admin_user(id=1),不迁移。
--   invitations      —— 管理员生成的一次性邀请(带有效期),被邀请者据此自填用户名/口令注册。
--   project_members  —— 项目与普通用户的多对多成员关系(管理员天然可见全部项目,不入此表)。
--   sessions 加列    —— role/user_id/username:会话携带身份,供权限校验与回显。
-- 既有会话行在 ALTER ... DEFAULT 后得到 role='admin',向后兼容。
-- 列类型与既有表对齐:users.id VARCHAR(64)、projects.id VARCHAR(64)、sessions.token VARCHAR(191)。

CREATE TABLE IF NOT EXISTS users (
    id            VARCHAR(64)  PRIMARY KEY,
    username      VARCHAR(255) NOT NULL UNIQUE,
    password_hash VARCHAR(255) NOT NULL,
    role          VARCHAR(16)  NOT NULL DEFAULT 'member',
    status        VARCHAR(16)  NOT NULL DEFAULT 'active',
    created_at    VARCHAR(32)  NOT NULL,
    updated_at    VARCHAR(32)  NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS invitations (
    id         VARCHAR(64)  PRIMARY KEY,
    token      VARCHAR(191) NOT NULL UNIQUE,
    created_by VARCHAR(255) NOT NULL,
    expires_at VARCHAR(32)  NOT NULL,
    used_at    VARCHAR(32),
    used_by    VARCHAR(64),
    created_at VARCHAR(32)  NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS project_members (
    project_id VARCHAR(64) NOT NULL,
    user_id    VARCHAR(64) NOT NULL,
    created_at VARCHAR(32) NOT NULL,
    PRIMARY KEY (project_id, user_id),
    FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE CASCADE,
    FOREIGN KEY (user_id)    REFERENCES users (id)    ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE INDEX idx_project_members_user ON project_members (user_id);

ALTER TABLE sessions ADD COLUMN role     VARCHAR(16)  NOT NULL DEFAULT 'admin';
ALTER TABLE sessions ADD COLUMN user_id  VARCHAR(64)  NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN username VARCHAR(255) NOT NULL DEFAULT '';
