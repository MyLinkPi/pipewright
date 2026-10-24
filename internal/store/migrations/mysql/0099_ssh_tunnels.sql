-- 0099 ssh_tunnels(MySQL):SSH 隧道配置。语义见 sqlite 同名迁移;user 为保留字需反引号。
CREATE TABLE IF NOT EXISTS ssh_tunnels (
    id            VARCHAR(64) PRIMARY KEY,
    name          VARCHAR(255) NOT NULL,
    server_id     VARCHAR(64) NULL,
    host          VARCHAR(255) NOT NULL DEFAULT '',
    port          BIGINT NOT NULL DEFAULT 22,
    `user`        VARCHAR(255) NOT NULL DEFAULT '',
    credential_id VARCHAR(64) NULL,
    mode          VARCHAR(16) NOT NULL,
    bind_host     VARCHAR(255) NOT NULL DEFAULT '127.0.0.1',
    bind_port     BIGINT NOT NULL DEFAULT 0,
    dest_host     VARCHAR(255) NOT NULL DEFAULT '',
    dest_port     BIGINT NOT NULL DEFAULT 0,
    enabled       BIGINT NOT NULL DEFAULT 1,
    created_at    VARCHAR(32) NOT NULL,
    updated_at    VARCHAR(32) NOT NULL,
    FOREIGN KEY (server_id) REFERENCES servers (id) ON DELETE RESTRICT,
    FOREIGN KEY (credential_id) REFERENCES credentials (id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE INDEX idx_ssh_tunnels_server_id ON ssh_tunnels (server_id);
CREATE INDEX idx_ssh_tunnels_credential_id ON ssh_tunnels (credential_id);
