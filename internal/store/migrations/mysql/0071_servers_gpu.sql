-- 0071 servers.gpu(MySQL):该机带 GPU(显卡机型)标记,语义见 sqlite 同名文件。
-- TINYINT(1) 存 0/1,与 SQLite 的 INTEGER 0/1 等价;默认 0(关闭)。
ALTER TABLE servers ADD COLUMN gpu TINYINT NOT NULL DEFAULT 0;