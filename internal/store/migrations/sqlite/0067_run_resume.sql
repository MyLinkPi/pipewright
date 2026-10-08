-- 0067 run resume:失败运行按节点恢复(派生重跑)。
-- additive only:只加列,不改既有列/语义;旧行两列均为空串 → 非恢复运行,行为不变。
--
--   resume_of_run_id : 本次运行由哪个(失败的)运行恢复而来;空串 = 普通运行(非派生)。
--                      溯源可见性:运行详情「恢复自 #xxxx」;链式恢复各自记直接父运行。
--   resume_plan_json : 恢复计划的节点动作数组(下标 = run_steps.ordinal),元素为
--                      inherit|run|retry|skip;空串 = 普通运行。执行器(dagrun)据它决定
--                      哪些节点继承为成功、哪些重跑、哪些人工跳过后放行下游。
ALTER TABLE pipeline_runs ADD COLUMN resume_of_run_id TEXT NOT NULL DEFAULT '';
ALTER TABLE pipeline_runs ADD COLUMN resume_plan_json TEXT NOT NULL DEFAULT '';
