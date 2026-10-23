-- 0098 ai_web_tools(SQLite):AI 配置加「网页搜索/抓取」工具配置。
--   web_enabled 总开关,默认关(按次计费且引入新出网行为,存量部署需显式打开);
--   web_provider 空 = 回退 LLM 配置(仅当 LLM 目的地为 DeepSeek 官方时可用),
--   首期枚举仅 'deepseek';web_base_url 空 = provider 官方默认;
--   web_api_key_ciphertext 为 vault 密文(与主密钥同规范,绝无明文入库)。
ALTER TABLE ai_config ADD COLUMN web_enabled INTEGER NOT NULL DEFAULT 0;
ALTER TABLE ai_config ADD COLUMN web_provider TEXT NOT NULL DEFAULT '';
ALTER TABLE ai_config ADD COLUMN web_base_url TEXT NOT NULL DEFAULT '';
ALTER TABLE ai_config ADD COLUMN web_api_key_ciphertext BLOB;
