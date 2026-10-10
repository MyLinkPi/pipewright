-- 0070 run_logs machine:日志行的来源机器归属(步骤 × 机器分组展示)。
-- additive only:只加列,不改既有列/语义;旧行均为空串 → 行为不变(前端单桶不显示机器切换)。
-- 值 = 机器显示名快照(部署/取机时刻);'' = 控制机本机执行或运行级日志。
-- 机器名非敏感(展示名,与 deploy_targets.server_name 同策略),不参与 mask 脱敏(脱敏只作用于 text)。
ALTER TABLE run_logs ADD COLUMN machine TEXT NOT NULL DEFAULT '';
