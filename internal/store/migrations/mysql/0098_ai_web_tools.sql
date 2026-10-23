-- 0098 ai_web_tools(MySQL):AI 配置加「网页搜索/抓取」工具配置(同 sqlite 0098)。
ALTER TABLE ai_config ADD COLUMN web_enabled TINYINT NOT NULL DEFAULT 0;
ALTER TABLE ai_config ADD COLUMN web_provider VARCHAR(255) NOT NULL DEFAULT '';
ALTER TABLE ai_config ADD COLUMN web_base_url VARCHAR(1024) NOT NULL DEFAULT '';
ALTER TABLE ai_config ADD COLUMN web_api_key_ciphertext BLOB;
