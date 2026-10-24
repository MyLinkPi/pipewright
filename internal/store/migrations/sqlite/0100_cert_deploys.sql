-- 0100 cert_deploys:证书部署目标(把证书/私钥下发到远程机器指定路径,部署后可执行
-- 命令如 nginx reload;证书签发/导入/续期后自动联动重新部署)。
--
--   cert_id     : 逻辑引用 certificates(不设 FK:证书删除由 certmgmt 层占用拦截,
--                 报人话错误,避免外键报错)
--   server_id   : FK → servers(RESTRICT:被部署目标引用的服务器不可删,防悬挂)
--   cert_path   : 远端证书(fullchain)绝对路径,部署后 chmod 644
--   key_path    : 远端私钥绝对路径,部署后 chmod 600(与 cert_path 不得相同)
--   post_command: 部署成功后在远端执行的 shell 命令(如 `nginx -s reload`),可空;
--                 root 登录直跑,否则经免密 sudo 提权执行
--   enabled     : 0 = 不参与自动联动部署(手动部署仍可用)
--   status      : '' | running | success | failed(最近一次部署结果)
--
-- cert_deploy_runs:部署历史(每次手动/自动部署一条;每目标保留最近 50 条,
-- 插入后边界法裁剪,防 detail 无限膨胀)。

CREATE TABLE IF NOT EXISTS cert_deploys (
    id              TEXT PRIMARY KEY,
    cert_id         TEXT NOT NULL,
    server_id       TEXT NOT NULL,
    name            TEXT NOT NULL DEFAULT '',
    cert_path       TEXT NOT NULL,
    key_path        TEXT NOT NULL,
    post_command    TEXT NOT NULL DEFAULT '',
    enabled         INTEGER NOT NULL DEFAULT 1,
    status          TEXT NOT NULL DEFAULT '',
    status_detail   TEXT NOT NULL DEFAULT '',
    last_deployed_at TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    FOREIGN KEY (server_id) REFERENCES servers (id) ON DELETE RESTRICT
);

CREATE INDEX IF NOT EXISTS idx_cert_deploys_cert_id ON cert_deploys (cert_id);
CREATE INDEX IF NOT EXISTS idx_cert_deploys_server_id ON cert_deploys (server_id);

CREATE TABLE IF NOT EXISTS cert_deploy_runs (
    id          TEXT PRIMARY KEY,
    deploy_id   TEXT NOT NULL,
    server_id   TEXT NOT NULL,
    server_name TEXT NOT NULL DEFAULT '',
    source      TEXT NOT NULL DEFAULT 'manual',
    status      TEXT NOT NULL,
    detail      TEXT NOT NULL DEFAULT '',
    started_at  TEXT NOT NULL,
    finished_at TEXT NOT NULL DEFAULT '',
    FOREIGN KEY (deploy_id) REFERENCES cert_deploys (id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_cert_deploy_runs_deploy_id ON cert_deploy_runs (deploy_id);
