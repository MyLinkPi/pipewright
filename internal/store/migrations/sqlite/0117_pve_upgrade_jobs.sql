-- 0117 pve_upgrade_jobs:PVE 大版本升级(8→9)滚动任务持久化。
-- 日常 apt 升级是内存态任务(照 createJob);大版本升级横跨 1-2 小时与多次节点重启,
-- 控制端自身重启后必须能识别「升了一半」的集群:状态懒加载时 running 无内存对应 →
-- interrupted,只允许人工 resume/retry,绝不自动续跑。逐节点状态机每步落库。
-- 集群删除由领域层显式连带清理(不设 cluster 外键,避免删除顺序耦合;口径同 0116)。
CREATE TABLE IF NOT EXISTS pve_upgrade_jobs (
    id           TEXT PRIMARY KEY,
    cluster_id   TEXT NOT NULL,
    kind         TEXT NOT NULL,
    status       TEXT NOT NULL,
    guest_policy TEXT NOT NULL,
    node_order   TEXT NOT NULL,
    preflight    TEXT NOT NULL DEFAULT '',
    error        TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_pve_upgrade_jobs_cluster ON pve_upgrade_jobs (cluster_id);
CREATE TABLE IF NOT EXISTS pve_upgrade_job_nodes (
    job_id      TEXT NOT NULL REFERENCES pve_upgrade_jobs (id) ON DELETE CASCADE,
    node        TEXT NOT NULL,
    seq         INTEGER NOT NULL,
    state       TEXT NOT NULL,
    error       TEXT NOT NULL DEFAULT '',
    started_at  TEXT NOT NULL DEFAULT '',
    finished_at TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (job_id, node)
);