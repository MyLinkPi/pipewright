-- 0112 本机角色(MySQL):结构同 SQLite 版(specs/local-node-roles.md)。
-- 注:MySQL 对 TEXT 列的默认值只接受表达式形式 DEFAULT ('...')(裸字面量报
-- ERROR 1101),与 0108 的括号惯例一致;本表统一用 VARCHAR 并带括号默认值。
CREATE TABLE IF NOT EXISTS local_node (
    id               VARCHAR(64) PRIMARY KEY,
    build_enabled    BIGINT NOT NULL DEFAULT 0,
    build_labels     VARCHAR(512) NOT NULL DEFAULT (''),
    build_max_builds BIGINT NOT NULL DEFAULT 0,
    build_priority   BIGINT NOT NULL DEFAULT 0,
    deploy_enabled   BIGINT NOT NULL DEFAULT 0,
    gateway_enabled  BIGINT NOT NULL DEFAULT 0,
    created_at       VARCHAR(32) NOT NULL,
    updated_at       VARCHAR(32) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
