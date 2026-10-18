-- 0083 mfa(SQLite):两步验证(TOTP)+ Passkey(WebAuthn)。
--   totp_credentials     —— 每身份一行的 TOTP 配置;secret 经 secretbox 加密落库
--                           (优先 vault master key,否则 system_config.mfa_secret_key 自动生成密钥)。
--   totp_backup_codes    —— 一次性恢复码,仅存 argon2id 哈希;验证通过即标记 used_at。
--   webauthn_credentials —— Passkey 凭据(每身份可多把,可命名设备);credential_id 唯一。
--   webauthn_challenges  —— 注册/登录挑战的服务端暂存(一次性,短 TTL),替代 cookie 会话方案。
--   sessions 加列        —— mfa_state:0=普通(7 天)/1=待二步验证(10 分钟)/2=已验证(30 天)。
--   system_config 加列   —— Passkey RP 配置(设置页在线改,即时生效)+ TOTP 密钥加密兜底密钥。
-- 身份统一 owner_key:管理员 "admin",普通用户 "user:<id>"(同 WebAuthn user handle)。

CREATE TABLE IF NOT EXISTS totp_credentials (
    owner_key   TEXT PRIMARY KEY,
    secret_enc  TEXT NOT NULL,
    enabled     INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS totp_backup_codes (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    owner_key  TEXT NOT NULL,
    code_hash  TEXT NOT NULL,
    used_at    TEXT,
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_totp_backup_codes_owner ON totp_backup_codes (owner_key);

CREATE TABLE IF NOT EXISTS webauthn_credentials (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    credential_id    TEXT NOT NULL UNIQUE,
    owner_key        TEXT NOT NULL,
    name             TEXT NOT NULL DEFAULT '',
    public_key       TEXT NOT NULL,
    attestation_type TEXT NOT NULL DEFAULT '',
    aaguid           TEXT NOT NULL DEFAULT '',
    sign_count       INTEGER NOT NULL DEFAULT 0,
    transports       TEXT NOT NULL DEFAULT '',
    clone_warning    INTEGER NOT NULL DEFAULT 0,
    user_present     INTEGER NOT NULL DEFAULT 0,
    user_verified    INTEGER NOT NULL DEFAULT 0,
    backup_eligible  INTEGER NOT NULL DEFAULT 0,
    backup_state     INTEGER NOT NULL DEFAULT 0,
    attachment       TEXT NOT NULL DEFAULT '',
    last_used_at     TEXT,
    created_at       TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_webauthn_credentials_owner ON webauthn_credentials (owner_key);

CREATE TABLE IF NOT EXISTS webauthn_challenges (
    id         TEXT PRIMARY KEY,
    kind       TEXT NOT NULL,
    data       TEXT NOT NULL,
    expires_at TEXT NOT NULL
);

ALTER TABLE sessions ADD COLUMN mfa_state INTEGER NOT NULL DEFAULT 0;

ALTER TABLE system_config ADD COLUMN webauthn_rp_id TEXT NOT NULL DEFAULT '';
ALTER TABLE system_config ADD COLUMN webauthn_origins TEXT NOT NULL DEFAULT '';
ALTER TABLE system_config ADD COLUMN passkey_login_enabled INTEGER NOT NULL DEFAULT 1;
ALTER TABLE system_config ADD COLUMN mfa_secret_key TEXT NOT NULL DEFAULT '';
