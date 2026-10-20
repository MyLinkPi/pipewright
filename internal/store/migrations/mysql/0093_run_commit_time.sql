-- 0093 runs(MySQL):DORA「变更前置时长」的真实提交时间。
--   pipeline_runs 新增 commit_time(RFC3339 文本,可空):执行器克隆解析出实际检出 commit 时
--   一并取该 commit 的提交时刻落库(经 run.Service.SetCommit 回写)。
--   老运行 / 未解析出 → NULL,dora 层回退用 created_at(入队时刻)作提交时间代理(旧行为)。
ALTER TABLE pipeline_runs ADD COLUMN commit_time VARCHAR(32) NULL;
