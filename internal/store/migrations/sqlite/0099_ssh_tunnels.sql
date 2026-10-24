-- 0099 ssh_tunnels:SSH 隧道配置(local -L / remote -R / dynamic -D)。
-- 隧道以独立 helper 子进程运行(同二进制 --tunnel-helper 模式),宿主重启不杀 helper
-- (孤儿化 + 心跳收养,对齐 mesh easytier 的 supervisor/reconcile 先例),故本表只存
-- 配置;运行态经 <dataDir>/tunnels/state/<id>.json 心跳文件汇报,绝不入库。
--
-- SSH 目标二选一:
--   server_id     : 绑定已登记服务器(FK → servers,ON DELETE RESTRICT:被隧道引用的
--                   服务器不可删,防悬挂;连接沿用其 host/port/user/凭据/跳板链)。
--                   NULL = 直连模式。
--   host/port/user/credential_id : 直连模式(支持不在服务器列表里的机器);
--                   credential_id FK → credentials(RESTRICT:被隧道引用不可删)。
--
--   mode          : local | remote | dynamic
--   bind_host     : 监听地址(local/dynamic = 本机;remote = 远端 sshd 侧),
--                   默认 127.0.0.1(安全默认,防 -D 代理裸奔)
--   bind_port     : 监听端口;0 = 自动分配(实际端口经心跳文件回显)
--   dest_host/port: local/remote 的转发目标(local = 远端侧可达地址;remote = 本机
--                   侧可达地址);dynamic 模式留空
--   enabled       : 兼作 autostart 与期望状态(0 = 停用并停进程)

CREATE TABLE IF NOT EXISTS ssh_tunnels (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    server_id     TEXT,
    host          TEXT NOT NULL DEFAULT '',
    port          INTEGER NOT NULL DEFAULT 22,
    user          TEXT NOT NULL DEFAULT '',
    credential_id TEXT,
    mode          TEXT NOT NULL,
    bind_host     TEXT NOT NULL DEFAULT '127.0.0.1',
    bind_port     INTEGER NOT NULL DEFAULT 0,
    dest_host     TEXT NOT NULL DEFAULT '',
    dest_port     INTEGER NOT NULL DEFAULT 0,
    enabled       INTEGER NOT NULL DEFAULT 1,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    FOREIGN KEY (server_id) REFERENCES servers (id) ON DELETE RESTRICT,
    FOREIGN KEY (credential_id) REFERENCES credentials (id) ON DELETE RESTRICT
);

CREATE INDEX IF NOT EXISTS idx_ssh_tunnels_server_id ON ssh_tunnels (server_id);
CREATE INDEX IF NOT EXISTS idx_ssh_tunnels_credential_id ON ssh_tunnels (credential_id);
