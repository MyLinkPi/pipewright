-- 0089 aliyun_ecs(MySQL):阿里云 ECS 批量导入(设置→阿里云 ECS)。
-- 口径同 sqlite 孪生:AccessKeyId 明文列可回显;AccessKeySecret 走 vault.SealSecret 密文 BLOB;
-- 实例映射表记录「ECS 实例 ↔ servers 行」,供再次导入时按实例 ID 更新到原机器行。
CREATE TABLE IF NOT EXISTS aliyun_ecs_accounts (
    id            VARCHAR(64)  PRIMARY KEY,
    name          VARCHAR(255) NOT NULL,
    access_key_id VARCHAR(255) NOT NULL,
    secret_sealed BLOB         NOT NULL,
    region        VARCHAR(64)  NOT NULL,
    created_at    VARCHAR(32)  NOT NULL,
    updated_at    VARCHAR(32)  NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS aliyun_ecs_instance_links (
    account_id  VARCHAR(64)  NOT NULL,
    instance_id VARCHAR(64)  NOT NULL,
    server_id   VARCHAR(64)  NOT NULL,
    updated_at  VARCHAR(32)  NOT NULL,
    PRIMARY KEY (account_id, instance_id),
    INDEX idx_aliyun_ecs_links_server (server_id),
    CONSTRAINT fk_aliyun_ecs_links_server FOREIGN KEY (server_id) REFERENCES servers (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
