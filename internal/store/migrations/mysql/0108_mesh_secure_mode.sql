-- 0108 mesh(MySQL):Secure Mode(全集群统一开关)——结构同 SQLite 版。
-- 注:MySQL 对 TEXT 列的默认值只接受表达式形式 DEFAULT ('...')(裸字面量报
-- ERROR 1101),与 0009/0045 的括号惯例一致。
-- secure_mode 默认 1:新部署默认开启;存量部署保持原值,经统一开关协议对齐。
ALTER TABLE mesh_settings ADD COLUMN secure_mode INTEGER NOT NULL DEFAULT 1;

CREATE TABLE IF NOT EXISTS mesh_mode_pending (
    id           INTEGER PRIMARY KEY,
    change_id    TEXT NOT NULL,
    action       TEXT NOT NULL,
    effective_at TEXT NOT NULL,
    initiated_by TEXT NOT NULL DEFAULT (''),
    acked_at     TEXT NOT NULL DEFAULT (''),
    applied_at   TEXT NOT NULL DEFAULT (''),
    aborted_at   TEXT NOT NULL DEFAULT ('')
);

ALTER TABLE mesh_acl ADD COLUMN switch_allowed TEXT NOT NULL DEFAULT ('[]');
