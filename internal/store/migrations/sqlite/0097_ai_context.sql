-- 0097 ai_context(SQLite):AI 配置加「上下文管理」声明 JSON。
--   context_json 形如 {"windowTokens":200000,"maxOutputTokens":8192,...};
--   0/缺省字段 = 用领域层默认(窗口 200K、输出 8192、历史 100 条/256KB、
--   自动压缩开、保留最近 15000 tokens、buffer 自动 max(10% 窗口, 窗口≥32K 时 16K))。
--   供 opschat agent 上下文预算/压缩策略消费;旧存量行默认 '{}' 全走默认值。
ALTER TABLE ai_config ADD COLUMN context_json TEXT NOT NULL DEFAULT '{}';
