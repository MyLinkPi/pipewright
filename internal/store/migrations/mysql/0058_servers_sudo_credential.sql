-- 0058 servers sudo credential(MySQL):服务器可选绑定一条 sudo_password 凭据。语义见 sqlite 同名文件。
ALTER TABLE servers ADD COLUMN sudo_credential_id VARCHAR(64) NOT NULL DEFAULT '';
