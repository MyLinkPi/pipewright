-- 0085 mesh_forward(SQLite):跨环境服务转发(经 mesh 把对端网关上注册的服务转发到本端网关)。
--   service_reg_services 新增 peer_node_id / peer_service_id:upstream_kind='forward' 专用
--     (指向对端 pipewright 节点上的服务 id),其余类型恒空;forward 服务无本地实例,
--     upstream/upstream_port 恒空,渲染成员来自对端目录(/api/mesh/servicereg/catalog)。
--   mesh_acl 新增 forward_allowed:第四张默认全拒绝授权单 —— 允许对端节点查询我的
--     服务目录并转发我的服务(网关 IP 清单随目录一并下发,供对端网关机配置 /32 路由)。

ALTER TABLE service_reg_services ADD COLUMN peer_node_id TEXT NOT NULL DEFAULT '';
ALTER TABLE service_reg_services ADD COLUMN peer_service_id TEXT NOT NULL DEFAULT '';

ALTER TABLE mesh_acl ADD COLUMN forward_allowed TEXT NOT NULL DEFAULT '[]';

-- forward_routes 是网关机 /32 路由的托管账本(JSON: server_id → [对端网关 IP]),
-- 只记录本平台下发的路由;调和删除仅针对账本内的目标,绝不触碰管理员手工路由。
ALTER TABLE service_reg_settings ADD COLUMN forward_routes TEXT NOT NULL DEFAULT '{}';
