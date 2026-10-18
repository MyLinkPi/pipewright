-- 0083 mfa(MySQL):两步验证(TOTP)+ Passkey(WebAuthn)。结构与 SQLite 版一致,
-- 列类型按既有表惯例对齐(RFC3339 时间串 VARCHAR(32)、owner_key VARCHAR(80))。
-- 身份统一 owner_key:管理员 "admin",普通用户 "user:<id>"(同 WebAuthn user handle)。

CREATE TABLE IF NOT EXISTS totp_credentials (
    owner_key   VARCHAR(80)  PRIMARY KEY,
    secret_enc  TEXT         NOT NULL,
    enabled     INT          NOT NULL DEFAULT 0,
    created_at  VARCHAR(32)  NOT NULL,
    updated_at  VARCHAR(32)  NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS totp_backup_codes (
    id         BIGINT       PRIMARY KEY AUTO_INCREMENT,
    owner_key  VARCHAR(80)  NOT NULL,
    code_hash  VARCHAR(255) NOT NULL,
    used_at    VARCHAR(32),
    created_at VARCHAR(32)  NOT NULL,
    INDEX idx_totp_backup_codes_owner (owner_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS webauthn_credentials (
    id               BIGINT       PRIMARY KEY AUTO_INCREMENT,
    credential_id    VARCHAR(512) NOT NULL UNIQUE,
    owner_key        VARCHAR(80)  NOT NULL,
    name             VARCHAR(255) NOT NULL DEFAULT '',
    public_key       TEXT         NOT NULL,
    attestation_type VARCHAR(255) NOT NULL DEFAULT '',
    aaguid           VARCHAR(255) NOT NULL DEFAULT '',
    sign_count       BIGINT       NOT NULL DEFAULT 0,
    transports       TEXT         NOT NULL,
    clone_warning    INT          NOT NULL DEFAULT 0,
    user_present     INT          NOT NULL DEFAULT 0,
    user_verified    INT          NOT NULL DEFAULT 0,
    backup_eligible  INT          NOT NULL DEFAULT 0,
    backup_state     INT          NOT NULL DEFAULT 0,
    attachment       VARCHAR(32)  NOT NULL DEFAULT '',
    last_used_at     VARCHAR(32),
    created_at       VARCHAR(32)  NOT NULL,
    INDEX idx_webauthn_credentials_owner (owner_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS webauthn_challenges (
    id         VARCHAR(64)  PRIMARY KEY,
    kind       VARCHAR(16)  NOT NULL,
    data       TEXT         NOT NULL,
    expires_at VARCHAR(32)  NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

ALTER TABLE sessions ADD COLUMN mfa_state INT NOT NULL DEFAULT 0;

ALTER TABLE system_config ADD COLUMN webauthn_rp_id VARCHAR(255) NOT NULL DEFAULT '';
ALTER TABLE system_config ADD COLUMN webauthn_origins VARCHAR(1024) NOT NULL DEFAULT '';
ALTER TABLE system_config ADD COLUMN passkey_login_enabled INT NOT NULL DEFAULT 1;
ALTER TABLE system_config ADD COLUMN mfa_secret_key VARCHAR(128) NOT NULL DEFAULT '';
