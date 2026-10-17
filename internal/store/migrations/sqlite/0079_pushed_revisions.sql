-- 0079 pushed_revisions(SQLite):被推送源的修订记录。
-- 每次对等推送成功落一行;items 为 JSON 数组:
--   [{name, kind: file|image, digest(sha256), size, destName, imageRef, workspacePath}]
-- file 条目的真字节存于制品库(内容寻址,天然去重);image 条目的 docker save 归档同样
-- 以 blob 入制品库,由推送接收侧导入本机镜像面后记录可用引用。

CREATE TABLE IF NOT EXISTS pushed_revisions (
    id             TEXT PRIMARY KEY,
    project_id     TEXT NOT NULL,
    label          TEXT NOT NULL DEFAULT '',
    origin_node_id TEXT NOT NULL DEFAULT '',
    origin_run_id  TEXT NOT NULL DEFAULT '',
    items          TEXT NOT NULL DEFAULT '[]',
    created_at     TEXT NOT NULL,
    FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_pushed_revisions_project ON pushed_revisions (project_id, created_at);
