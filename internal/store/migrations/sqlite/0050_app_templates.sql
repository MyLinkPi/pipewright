-- 0050 app_templates:应用商店模板(DPanel 式一键部署)。
-- 模板 = docker-compose YAML + 参数 schema;部署复用 Stacks 受管链路(/opt/pipewright/stacks)。
-- builtin=1 为内置模板(启动幂等 seed,不可删除);builtin=0 为用户自定义。
-- params_json 形如 [{"name":"root_password","type":"secret","required":true,"autoGenerate":true}]。
CREATE TABLE IF NOT EXISTS app_templates (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL DEFAULT '',
    description  TEXT NOT NULL DEFAULT '',
    icon         TEXT NOT NULL DEFAULT '',
    compose_yaml TEXT NOT NULL,
    params_json  TEXT NOT NULL DEFAULT '[]',
    builtin      INTEGER NOT NULL DEFAULT 0,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);
