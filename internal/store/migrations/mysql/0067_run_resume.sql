-- 0067 run resume(MySQL):失败运行按节点恢复(派生重跑)。语义见 sqlite 同名文件。
ALTER TABLE pipeline_runs ADD COLUMN resume_of_run_id VARCHAR(64) NOT NULL DEFAULT '';
ALTER TABLE pipeline_runs ADD COLUMN resume_plan_json LONGTEXT NOT NULL DEFAULT ('');
