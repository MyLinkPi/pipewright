-- 0077 mesh(SQLite):集群——对等节点 + EasyTier 自组网。
--   mesh_settings —— 本节点身份与组网配置(单行,id 恒为 1,首用惰性生成):
--     node_id 是 ACL 授权与「被推送源项目」一一对应(origin)的锚点;
--     private_key 为 ed25519 私钥经 vault.SealSecret 的密文 BLOB(绝无明文/PEM 落库),
--     public_key 为对应公钥 hex(可公开,随 /api/mesh/hello 发布)。
--     network_secret 同为 SealSecret 密文(EasyTier 组网密钥,回显时只报 hasSecret)。
--   mesh_peers   —— 已知对等节点注册表:发现循环(auto)或手工补录(manual)upsert。
--   mesh_acl     —— 三张默认全拒绝的授权单(单行,服务层惰性补建):
--     visible_to(可见我的服务器/项目/运行)、push_allowed(允许向我推送产物/镜像)、
--     trigger_allowed(允许启动我的工作流);均存 JSON 数组 [node_id]。

CREATE TABLE IF NOT EXISTS mesh_settings (
    id             INTEGER PRIMARY KEY CHECK (id = 1),
    node_id        TEXT NOT NULL,
    name           TEXT NOT NULL DEFAULT '',
    public_key     TEXT NOT NULL DEFAULT '',
    private_key    BLOB,
    enabled        INTEGER NOT NULL DEFAULT 0,
    api_port       INTEGER NOT NULL DEFAULT 0,
    easytier_mode  TEXT NOT NULL DEFAULT 'managed',
    easytier_rpc   TEXT NOT NULL DEFAULT '127.0.0.1:15888',
    network_name   TEXT NOT NULL DEFAULT '',
    network_secret BLOB,
    virtual_ip     TEXT NOT NULL DEFAULT '',
    peers          TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS mesh_peers (
    node_id    TEXT PRIMARY KEY,
    name       TEXT NOT NULL DEFAULT '',
    virtual_ip TEXT NOT NULL DEFAULT '',
    api_port   INTEGER NOT NULL DEFAULT 0,
    public_key TEXT NOT NULL DEFAULT '',
    version    TEXT NOT NULL DEFAULT '',
    source     TEXT NOT NULL DEFAULT 'auto',
    public_url TEXT NOT NULL DEFAULT '',
    last_seen  TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_mesh_peers_virtual_ip ON mesh_peers (virtual_ip);

CREATE TABLE IF NOT EXISTS mesh_acl (
    id              INTEGER PRIMARY KEY CHECK (id = 1),
    visible_to      TEXT NOT NULL DEFAULT '[]',
    push_allowed    TEXT NOT NULL DEFAULT '[]',
    trigger_allowed TEXT NOT NULL DEFAULT '[]',
    updated_at      TEXT NOT NULL DEFAULT ''
);
