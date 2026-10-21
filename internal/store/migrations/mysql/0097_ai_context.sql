-- 0097 ai_context(MySQL):AI 配置加「上下文管理」声明 JSON(同 sqlite 0097)。
ALTER TABLE ai_config ADD COLUMN context_json LONGTEXT NOT NULL DEFAULT ('{}');
