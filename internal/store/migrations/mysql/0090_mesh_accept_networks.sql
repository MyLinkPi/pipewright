-- 0090 mesh(MySQL):集群路由隔离 —— 管理员自定义通告网段不再被任何环境自动路由
--   (跨环境网段重叠 = 生产事故;自动路由仅限代码生成的各环境网关 /32)。
--   mesh_settings 新增 accept_networks_custom:本节点显式接受的跨环境通告网段
--   (路由白名单的「手动确认」项,默认空)。
ALTER TABLE mesh_settings ADD COLUMN accept_networks_custom TEXT NOT NULL DEFAULT ('');
