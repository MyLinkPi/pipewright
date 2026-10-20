-- 0092 firewall(MySQL):集群防火墙访问限制 —— 两步生效(表语义同 SQLite 0092)。
--   TEXT 主键改 VARCHAR;快照/草稿列保持 TEXT(逗号/换行分隔,与 mesh peers 同模式)。

CREATE TABLE IF NOT EXISTS firewall_guard (
    id                 VARCHAR(32)  PRIMARY KEY,
    ports              VARCHAR(255) NOT NULL DEFAULT '',
    extra_cidrs        TEXT         NOT NULL,
    phase              VARCHAR(16)  NOT NULL DEFAULT 'off',
    pending_ports      VARCHAR(255) NOT NULL DEFAULT '',
    pending_cidrs      TEXT         NOT NULL,
    pending_applied_at VARCHAR(32)  NOT NULL DEFAULT '',
    confirmed_ports    VARCHAR(255) NOT NULL DEFAULT '',
    confirmed_cidrs    TEXT         NOT NULL,
    last_result        VARCHAR(16)  NOT NULL DEFAULT '',
    status_detail      TEXT         NOT NULL,
    last_applied_at    VARCHAR(32)  NOT NULL DEFAULT '',
    created_at         VARCHAR(32)  NOT NULL DEFAULT '',
    updated_at         VARCHAR(32)  NOT NULL DEFAULT '',
    CONSTRAINT chk_firewall_guard_singleton CHECK (id = 'default')
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
