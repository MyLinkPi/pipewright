-- 0112 本机角色(SQLite):控制节点可选启用为 构建机 / 部署目标 / 网关主机
-- (specs/local-node-roles.md)。单行表(仿 mesh_settings,id 恒 'local'),默认全关 ——
-- 关闭态行为逐字节不变;本机永不进 servers 表(不变量保留,ErrLocalHostImmutable 语义不变)。
--   build_*       构建机池参数,与 servers.labels/max_builds/priority 同语义:
--                 labels 空 = 不参与标签圈选(仍可被 server:__local__ 钉死);
--                 max_builds 0 = 调度器全局默认;priority 0-100。
--   deploy/gateway_enabled 仅 Linux 控制机可开(领域层校验,非 Linux 保存 → 422)。
CREATE TABLE IF NOT EXISTS local_node (
    id               TEXT PRIMARY KEY,
    build_enabled    INTEGER NOT NULL DEFAULT 0,
    build_labels     TEXT    NOT NULL DEFAULT '',
    build_max_builds INTEGER NOT NULL DEFAULT 0,
    build_priority   INTEGER NOT NULL DEFAULT 0,
    deploy_enabled   INTEGER NOT NULL DEFAULT 0,
    gateway_enabled  INTEGER NOT NULL DEFAULT 0,
    created_at       TEXT    NOT NULL,
    updated_at       TEXT    NOT NULL
);
