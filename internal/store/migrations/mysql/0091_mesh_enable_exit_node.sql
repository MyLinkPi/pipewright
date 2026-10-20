-- 0091 mesh(MySQL):出口节点 —— 平台节点可显式允许充当客户端本地代理的出口
--   (--enable-exit-node;foreign network 经 network secret 校验,默认关闭)。
ALTER TABLE mesh_settings ADD COLUMN enable_exit_node INTEGER NOT NULL DEFAULT 0;
