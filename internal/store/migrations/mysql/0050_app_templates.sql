-- 0050 app_templates(MySQL):应用商店模板(DPanel 式一键部署)。
-- 模板 = docker-compose YAML + 参数 schema;部署复用 Stacks 受管链路。builtin=1 不可删除。
CREATE TABLE IF NOT EXISTS app_templates (
    id           VARCHAR(64) PRIMARY KEY,
    name         VARCHAR(128) NOT NULL,
    display_name VARCHAR(255) NOT NULL DEFAULT '',
    description  VARCHAR(2048) NOT NULL DEFAULT '',
    icon         VARCHAR(16) NOT NULL DEFAULT '',
    compose_yaml TEXT NOT NULL,
    params_json  TEXT NOT NULL,
    builtin      INT NOT NULL DEFAULT 0,
    created_at   VARCHAR(32) NOT NULL,
    updated_at   VARCHAR(32) NOT NULL,
    UNIQUE KEY uq_app_templates_name (name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
