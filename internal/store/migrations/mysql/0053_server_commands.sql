-- 0053 server_commands(MySQL):批量执行命令(语义见 sqlite 同名文件)。
CREATE TABLE IF NOT EXISTS server_command_runs (
    id         VARCHAR(64) PRIMARY KEY,
    command    TEXT NOT NULL,
    total      INT NOT NULL,
    ok_count   INT NOT NULL,
    fail_count INT NOT NULL,
    created_at VARCHAR(32) NOT NULL
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;
CREATE TABLE IF NOT EXISTS server_command_results (
    id          BIGINT AUTO_INCREMENT PRIMARY KEY,
    run_id      VARCHAR(64) NOT NULL,
    server_id   VARCHAR(64) NOT NULL,
    server_name VARCHAR(255) NOT NULL DEFAULT '',
    ok          INT NOT NULL,
    exit_code   INT NOT NULL,
    stdout      MEDIUMTEXT NOT NULL,
    stderr      MEDIUMTEXT NOT NULL,
    error       VARCHAR(1024) NOT NULL DEFAULT '',
    duration_ms INT NOT NULL DEFAULT 0,
    INDEX idx_server_command_results_run (run_id),
    CONSTRAINT fk_server_command_results_run FOREIGN KEY (run_id) REFERENCES server_command_runs (id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;
