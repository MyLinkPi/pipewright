-- 0087 mesh_client_networks(MySQL):集群客户端(Win11 pwclient)——自定义子网代理通告
-- (表语义同 SQLite 0087)。

ALTER TABLE mesh_settings ADD COLUMN proxy_networks_custom TEXT NOT NULL DEFAULT '';
