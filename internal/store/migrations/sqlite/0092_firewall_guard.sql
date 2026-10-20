-- 0092 firewall(SQLite):集群防火墙访问限制 —— 两步生效(临时 → 确认固化)。
--   firewall_guard 单行表(id 恒为 'default',服务层 getOrCreate 兜底):
--     ports/extra_cidrs        —— 草稿(下次 apply 的参数;保存不碰机器);
--     phase                    —— 'off' | 'pending' | 'active'(临时生效待确认 / 已固化);
--     pending_ports/pending_cidrs/pending_applied_at —— 临时生效快照(confirm 固化的
--                                永远是已写入内核的这份,不吃编辑中的草稿;超时/重启回退依据);
--     confirmed_ports/confirmed_cidrs —— 固化版快照(KeepAlive 跨重启断言的唯一来源;
--                                机器重启后只重放本列 = 「回退到未开或上一个版本的规则」);
--     last_result/status_detail/last_applied_at —— 最近一次应用/回退结果(人话)。

CREATE TABLE IF NOT EXISTS firewall_guard (
    id                 TEXT PRIMARY KEY CHECK (id = 'default'),
    ports              TEXT NOT NULL DEFAULT '',
    extra_cidrs        TEXT NOT NULL DEFAULT '',
    phase              TEXT NOT NULL DEFAULT 'off',
    pending_ports      TEXT NOT NULL DEFAULT '',
    pending_cidrs      TEXT NOT NULL DEFAULT '',
    pending_applied_at TEXT NOT NULL DEFAULT '',
    confirmed_ports    TEXT NOT NULL DEFAULT '',
    confirmed_cidrs    TEXT NOT NULL DEFAULT '',
    last_result        TEXT NOT NULL DEFAULT '',
    status_detail      TEXT NOT NULL DEFAULT '',
    last_applied_at    TEXT NOT NULL DEFAULT '',
    created_at         TEXT NOT NULL DEFAULT '',
    updated_at         TEXT NOT NULL DEFAULT ''
);
