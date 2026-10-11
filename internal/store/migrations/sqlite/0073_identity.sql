-- 0073 identity(SQLite):普通用户体系。
--   users            —— 普通用户(member)账号;管理员仍存于 admin_user(id=1),不迁移。
--   invitations      —— 管理员生成的一次性邀请(带有效期),被邀请者据此自填用户名/口令注册。
--   project_members  —— 项目与普通用户的多对多成员关系(管理员天然可见全部项目,不入此表)。
--   sessions 加列    —— role/user_id/username:会话携带身份,供权限校验与回显。
-- 既有会话行在 ALTER ... DEFAULT 后得到 role='admin',向后兼容。

CREATE TABLE IF NOT EXISTS users (
    id            TEXT PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    role          TEXT NOT NULL DEFAULT 'member',
    status        TEXT NOT NULL DEFAULT 'active',
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS invitations (
    id         TEXT PRIMARY KEY,
    token      TEXT NOT NULL UNIQUE,
    created_by TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    used_at    TEXT,
    used_by    TEXT,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS project_members (
    project_id TEXT NOT NULL,
    user_id    TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (project_id, user_id),
    FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE CASCADE,
    FOREIGN KEY (user_id)    REFERENCES users (id)    ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_project_members_user ON project_members (user_id);

ALTER TABLE sessions ADD COLUMN role     TEXT NOT NULL DEFAULT 'admin';
ALTER TABLE sessions ADD COLUMN user_id  TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN username TEXT NOT NULL DEFAULT '';
