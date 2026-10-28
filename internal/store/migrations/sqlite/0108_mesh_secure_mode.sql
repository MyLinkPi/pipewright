-- 0108 mesh(SQLite):Secure Mode(全集群统一开关)——
--   mesh_settings.secure_mode:本节点 easytier 是否以 --secure-mode 运行(默认 1:
--     新部署默认开启;存量部署保持原值,经统一开关协议对齐)。
--     该列几乎总由 mesh_mode_pending 协议在约定时刻翻转,PUT settings 的直改仅作
--     恢复舱口(单节点手动对齐);
--   mesh_mode_pending:统一开关协议的本地暂存(每节点单行,最新覆盖):
--     发起节点 fan-out 下发 change 后各节点落此行并应答,effective_at 到点由本地
--     对账节拍翻转 secure_mode(不依赖再通信,时钟偏差者晚切自愈);
--   mesh_acl.switch_allowed:第六张默认全拒绝授权单 —— 允许对端节点向我下发/
--     撤销 Secure Mode 切换指令(统一开关的入站端点鉴权;发起节点须先获各对端授权)。
ALTER TABLE mesh_settings ADD COLUMN secure_mode INTEGER NOT NULL DEFAULT 1;

CREATE TABLE IF NOT EXISTS mesh_mode_pending (
    id           INTEGER PRIMARY KEY CHECK (id = 1),
    change_id    TEXT NOT NULL,
    action       TEXT NOT NULL,
    effective_at TEXT NOT NULL,
    initiated_by TEXT NOT NULL DEFAULT '',
    acked_at     TEXT NOT NULL DEFAULT '',
    applied_at   TEXT NOT NULL DEFAULT '',
    aborted_at   TEXT NOT NULL DEFAULT ''
);

ALTER TABLE mesh_acl ADD COLUMN switch_allowed TEXT NOT NULL DEFAULT '[]';
