-- 0101 security(MySQL):安全中心三表(表语义同 SQLite 0101)。
-- TEXT 改 VARCHAR;token_sealed 为 varbinary 密文 BLOB(vault.SealSecret 产出)。

CREATE TABLE IF NOT EXISTS scanner_instances (
    id            VARCHAR(64)  PRIMARY KEY,
    kind          VARCHAR(32)  NOT NULL,
    name          VARCHAR(255) NOT NULL,
    host          VARCHAR(255) NOT NULL,
    port          INT          NOT NULL,
    token_sealed  VARBINARY(512) NOT NULL DEFAULT '',
    source        VARCHAR(16)  NOT NULL DEFAULT 'manual',
    server_id     VARCHAR(64)  NOT NULL DEFAULT '',
    stack_name    VARCHAR(128) NOT NULL DEFAULT '',
    created_at    VARCHAR(32)  NOT NULL,
    updated_at    VARCHAR(32)  NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE UNIQUE INDEX idx_scanner_instances_endpoint ON scanner_instances (kind, host, port);

CREATE TABLE IF NOT EXISTS scan_runs (
    id                  VARCHAR(64)  PRIMARY KEY,
    server_id           VARCHAR(64)  NOT NULL,
    server_name         VARCHAR(255) NOT NULL DEFAULT '',
    scan_type           VARCHAR(16)  NOT NULL,
    status              VARCHAR(16)  NOT NULL,
    trigger_source      VARCHAR(16)  NOT NULL DEFAULT 'manual',
    summary             TEXT         NOT NULL,
    artifact_key        VARCHAR(128) NOT NULL DEFAULT '',
    error               TEXT         NOT NULL,
    scanner_instance_id VARCHAR(64)  NOT NULL DEFAULT '',
    started_at          VARCHAR(32)  NOT NULL,
    finished_at         VARCHAR(32)  NOT NULL DEFAULT ''
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE INDEX idx_scan_runs_server ON scan_runs (server_id, started_at);
CREATE INDEX idx_scan_runs_status ON scan_runs (status);

CREATE TABLE IF NOT EXISTS security_schedule (
    id          VARCHAR(32)  PRIMARY KEY,
    enabled     INT          NOT NULL DEFAULT 0,
    expression  VARCHAR(64)  NOT NULL DEFAULT '0 3 * * *',
    scan_types  VARCHAR(128) NOT NULL DEFAULT '["trivy"]',
    last_run_at VARCHAR(32)  NOT NULL DEFAULT '',
    updated_at  VARCHAR(32)  NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
