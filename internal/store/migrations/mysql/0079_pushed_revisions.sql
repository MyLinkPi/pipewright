-- 0079 pushed_revisions(MySQL):被推送源的修订记录(语义同 SQLite 0079)。

CREATE TABLE IF NOT EXISTS pushed_revisions (
    id             VARCHAR(64)  PRIMARY KEY,
    project_id     VARCHAR(64)  NOT NULL,
    label          VARCHAR(255) NOT NULL DEFAULT '',
    origin_node_id VARCHAR(64)  NOT NULL DEFAULT '',
    origin_run_id  VARCHAR(64)  NOT NULL DEFAULT '',
    items          TEXT         NOT NULL,
    created_at     VARCHAR(32)  NOT NULL,
    FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE INDEX idx_pushed_revisions_project ON pushed_revisions (project_id, created_at);
