-- 0100 cert_deploys(MySQL):证书部署目标 + 部署历史。语义见 sqlite 同名迁移。
CREATE TABLE IF NOT EXISTS cert_deploys (
    id              VARCHAR(64) PRIMARY KEY,
    cert_id         VARCHAR(64) NOT NULL,
    server_id       VARCHAR(64) NOT NULL,
    name            VARCHAR(255) NOT NULL DEFAULT '',
    cert_path       VARCHAR(512) NOT NULL,
    key_path        VARCHAR(512) NOT NULL,
    post_command    TEXT NOT NULL,
    enabled         BIGINT NOT NULL DEFAULT 1,
    status          VARCHAR(16) NOT NULL DEFAULT '',
    status_detail   TEXT NOT NULL,
    last_deployed_at VARCHAR(32) NOT NULL DEFAULT '',
    created_at      VARCHAR(32) NOT NULL,
    updated_at      VARCHAR(32) NOT NULL,
    FOREIGN KEY (server_id) REFERENCES servers (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE INDEX idx_cert_deploys_cert_id ON cert_deploys (cert_id);
CREATE INDEX idx_cert_deploys_server_id ON cert_deploys (server_id);

CREATE TABLE IF NOT EXISTS cert_deploy_runs (
    id          VARCHAR(64) PRIMARY KEY,
    deploy_id   VARCHAR(64) NOT NULL,
    server_id   VARCHAR(64) NOT NULL,
    server_name VARCHAR(255) NOT NULL DEFAULT '',
    source      VARCHAR(16) NOT NULL DEFAULT 'manual',
    status      VARCHAR(16) NOT NULL,
    detail      TEXT NOT NULL,
    started_at  VARCHAR(32) NOT NULL,
    finished_at VARCHAR(32) NOT NULL DEFAULT '',
    FOREIGN KEY (deploy_id) REFERENCES cert_deploys (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE INDEX idx_cert_deploy_runs_deploy_id ON cert_deploy_runs (deploy_id);
