-- 0101 security(SQLite):安全中心——扫描器登记表、扫描运行记录、定时规则。
--   scanner_instances:trivy-server 等中心扫描服务接入点;token_sealed 为可选
--   访问令牌经 vault.SealSecret 的密文 BLOB(绝无明文落库/出响应);
--   source=appstore 表示应用市场部署 trivy-server 模板后自动登记,
--   kind+host+port 唯一(重新部署同地址原地轮换令牌,不产生重复条目)。
--   scan_runs:一次扫描(单机单类型)的运行记录;summary 为 JSON 文本
--   (各级别 finding 计数);artifact_key 为完整报告 JSON 在 artifact store 的
--   内容寻址键(明细按需读取,避免巨型 JSON 进关系表)。
--   security_schedule:单行(id='default')的定时扫描规则;expression 为 5 段
--   cron(经 internal/cron Parse 校验);scan_types 为 JSON 数组文本(["trivy","nuclei"])。

CREATE TABLE IF NOT EXISTS scanner_instances (
    id            TEXT PRIMARY KEY,
    kind          TEXT NOT NULL,
    name          TEXT NOT NULL,
    host          TEXT NOT NULL,
    port          INTEGER NOT NULL,
    token_sealed  BLOB NOT NULL DEFAULT x'',
    source        TEXT NOT NULL DEFAULT 'manual',
    server_id     TEXT NOT NULL DEFAULT '',
    stack_name    TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_scanner_instances_endpoint ON scanner_instances (kind, host, port);

CREATE TABLE IF NOT EXISTS scan_runs (
    id                  TEXT PRIMARY KEY,
    server_id           TEXT NOT NULL,
    server_name         TEXT NOT NULL DEFAULT '',
    scan_type           TEXT NOT NULL,
    status              TEXT NOT NULL,
    trigger_source      TEXT NOT NULL DEFAULT 'manual',
    summary             TEXT NOT NULL DEFAULT '',
    artifact_key        TEXT NOT NULL DEFAULT '',
    error               TEXT NOT NULL DEFAULT '',
    scanner_instance_id TEXT NOT NULL DEFAULT '',
    started_at          TEXT NOT NULL,
    finished_at         TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_scan_runs_server ON scan_runs (server_id, started_at);
CREATE INDEX IF NOT EXISTS idx_scan_runs_status ON scan_runs (status);

CREATE TABLE IF NOT EXISTS security_schedule (
    id          TEXT PRIMARY KEY,
    enabled     INTEGER NOT NULL DEFAULT 0,
    expression  TEXT NOT NULL DEFAULT '0 3 * * *',
    scan_types  TEXT NOT NULL DEFAULT '["trivy"]',
    last_run_at TEXT NOT NULL DEFAULT '',
    updated_at  TEXT NOT NULL
);
