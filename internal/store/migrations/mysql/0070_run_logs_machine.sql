-- 0070 run_logs machine(MySQL):日志行的来源机器归属。语义见 sqlite 同名文件。
ALTER TABLE run_logs ADD COLUMN machine VARCHAR(255) NOT NULL DEFAULT '';
