-- 0106 pipeline_settings.pac_vars_json(MySQL):流水线即代码变量声明 + 填写值(语义同 sqlite 版)。
ALTER TABLE pipeline_settings ADD COLUMN pac_vars_json LONGTEXT NOT NULL DEFAULT ('');
