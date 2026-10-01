-- 0057 system_config:系统级运行时配置单例(id=1)。
-- public_url 是平台对外访问地址(如 https://ci.example.com),供通知里的签名审批链接与
-- PR 状态回写的 target_url 使用;空 = 未配置(相关外链优雅关闭)。
-- 取代环境变量 PIPEWRIGHT_PUBLIC_URL(运行时可改,无需重启)。
CREATE TABLE IF NOT EXISTS system_config (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    public_url  TEXT NOT NULL DEFAULT '',
    updated_at  TEXT NOT NULL DEFAULT ''
);
INSERT OR IGNORE INTO system_config (id, public_url, updated_at) VALUES (1, '', '');
