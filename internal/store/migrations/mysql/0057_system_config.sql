-- 0057 system_config(MySQL):系统级运行时配置单例。public_url 取代环境变量
-- PIPEWRIGHT_PUBLIC_URL(通知审批链接 / PR 回写 target_url;空 = 关闭)。
CREATE TABLE IF NOT EXISTS system_config (
    id          INT PRIMARY KEY,
    public_url  VARCHAR(512) NOT NULL DEFAULT '',
    updated_at  VARCHAR(32) NOT NULL DEFAULT ''
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
INSERT IGNORE INTO system_config (id, public_url, updated_at) VALUES (1, '', '');
