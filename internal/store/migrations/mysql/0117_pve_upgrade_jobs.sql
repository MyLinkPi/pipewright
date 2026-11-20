-- 0117 pve_upgrade_jobs(MySQL):PVE 大版本升级(8→9)滚动任务持久化。
-- 口径同 sqlite 孪生:日常升级内存态,大版本升级逐节点状态机每步落库;控制端重启后
-- running 无内存对应 → interrupted,只允许人工 resume/retry。集群删除领域层连带清理。
CREATE TABLE IF NOT EXISTS pve_upgrade_jobs (
    id           VARCHAR(64)  NOT NULL,
    cluster_id   VARCHAR(64)  NOT NULL,
    kind         VARCHAR(32)  NOT NULL,
    status       VARCHAR(32)  NOT NULL,
    guest_policy VARCHAR(32)  NOT NULL,
    node_order   TEXT         NOT NULL,
    preflight    TEXT         NOT NULL,
    error        TEXT         NOT NULL,
    created_at   VARCHAR(32)  NOT NULL,
    updated_at   VARCHAR(32)  NOT NULL,
    PRIMARY KEY (id),
    INDEX idx_pve_upgrade_jobs_cluster (cluster_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
CREATE TABLE IF NOT EXISTS pve_upgrade_job_nodes (
    job_id      VARCHAR(64)  NOT NULL,
    node        VARCHAR(64)  NOT NULL,
    seq         BIGINT       NOT NULL,
    state       VARCHAR(32)  NOT NULL,
    error       TEXT         NOT NULL,
    started_at  VARCHAR(32)  NOT NULL DEFAULT '',
    finished_at VARCHAR(32)  NOT NULL DEFAULT '',
    PRIMARY KEY (job_id, node),
    CONSTRAINT fk_pve_upgrade_job_nodes_job FOREIGN KEY (job_id) REFERENCES pve_upgrade_jobs (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;