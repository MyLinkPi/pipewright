-- 0087 mesh_client_networks(SQLite):集群客户端(Win11 pwclient)——自定义子网代理通告。
--   mesh_settings 新增 proxy_networks_custom:管理员手工配置的「本节点可达、需通告进
--     集群虚拟网络」的网段列表(cidr,换行分隔存储,与 peers 同模式)。与 servicereg
--     自动产出的网关 /32 合并后作为 easytier 子网代理参数(--proxy-networks)传入;
--     EasyTier 默认启用内置用户态 NAT,被代理网段内设备无需回程路由即可返程。
--   客户端在导出的加入配置(pipewright-client.yaml)辅助下组网,并可经 --manual-routes
--     自由限定本机路由哪些通告网段。

ALTER TABLE mesh_settings ADD COLUMN proxy_networks_custom TEXT NOT NULL DEFAULT '';
