-- 0114 run_commit_message:运行详情展示 commit message。
-- 克隆解析出实际检出 commit 时,顺带回写该 commit 的 subject(首行)到 trigger_commit_message,
-- 运行详情 meta 区在短 SHA 旁展示;老运行/未解析出为空串(前端不渲染该行)。
ALTER TABLE pipeline_runs ADD COLUMN trigger_commit_message TEXT NOT NULL DEFAULT '';
