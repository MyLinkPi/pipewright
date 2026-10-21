-- 0095 meshdns(MySQL):集群 DNS 映射 —— 指定域名 A 记录自动指向本机虚拟 IP。
--   fqdn 全局唯一(一个域名只映射一次);last_* 三列是 Syncer 回写的最近同步状态。
CREATE TABLE IF NOT EXISTS mesh_dns_records (
  id VARCHAR(36) PRIMARY KEY,
  provider_id VARCHAR(36) NOT NULL,
  fqdn VARCHAR(253) NOT NULL,
  enabled INT NOT NULL DEFAULT 1,
  last_ip VARCHAR(64) NOT NULL DEFAULT '',
  last_synced_at VARCHAR(32) NOT NULL DEFAULT '',
  last_error VARCHAR(512) NOT NULL DEFAULT '',
  created_at VARCHAR(32) NOT NULL,
  updated_at VARCHAR(32) NOT NULL,
  UNIQUE(fqdn)
);
