-- 0114 run_commit_message(MySQL):运行详情展示 commit message,pipeline_runs 增 trigger_commit_message 列。
ALTER TABLE pipeline_runs ADD COLUMN trigger_commit_message LONGTEXT NOT NULL DEFAULT ('');
