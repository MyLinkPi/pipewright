-- 0052 runner pool(MySQL):多节点构建机池 + 标签调度(FR-8-14 续 / FR-8-19)。语义见 sqlite 同名文件。
ALTER TABLE servers ADD COLUMN labels VARCHAR(512) NOT NULL DEFAULT '';
ALTER TABLE servers ADD COLUMN max_builds INT NOT NULL DEFAULT 1;
ALTER TABLE servers ADD COLUMN priority INT NOT NULL DEFAULT 0;
ALTER TABLE project_runners ADD COLUMN selector VARCHAR(255) NOT NULL DEFAULT '';
UPDATE project_runners SET selector = CONCAT('server:', runner_server_id) WHERE runner_server_id <> '';
