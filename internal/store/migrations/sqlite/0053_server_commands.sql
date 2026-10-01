-- 0053 server_commands:批量执行命令(服务器状态页 → 勾选多机 → 同步执行 + 历史回看)。
-- 一次批量执行 = 一行 runs(头部:命令 + 成败计数)+ N 行 results(逐机:退出码/stdout/stderr)。
-- 输出各截断 64KiB 落库;runs 仅保留最近 200 次(防输出 blob 无限膨胀),更早的连带逐机结果删除。
-- created_at 与 audit_log 同约定:TEXT、UTC、RFC3339Nano 字串序即时间序。
CREATE TABLE IF NOT EXISTS server_command_runs (
    id         TEXT PRIMARY KEY,
    command    TEXT NOT NULL,
    total      INTEGER NOT NULL,
    ok_count   INTEGER NOT NULL,
    fail_count INTEGER NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS server_command_results (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id      TEXT NOT NULL,
    server_id   TEXT NOT NULL,
    server_name TEXT NOT NULL DEFAULT '',
    ok          INTEGER NOT NULL,
    exit_code   INTEGER NOT NULL,
    stdout      TEXT NOT NULL DEFAULT '',
    stderr      TEXT NOT NULL DEFAULT '',
    error       TEXT NOT NULL DEFAULT '',
    duration_ms INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (run_id) REFERENCES server_command_runs (id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_server_command_results_run ON server_command_results (run_id);
