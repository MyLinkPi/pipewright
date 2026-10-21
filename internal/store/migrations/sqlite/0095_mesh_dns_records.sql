-- 0095 meshdns(SQLite):集群 DNS 映射 —— 指定域名 A 记录自动指向本机虚拟 IP。
--   fqdn 全局唯一(一个域名只映射一次);last_* 三列是 Syncer 回写的最近同步状态。
CREATE TABLE IF NOT EXISTS mesh_dns_records (
  id TEXT PRIMARY KEY,
  provider_id TEXT NOT NULL,
  fqdn TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1,
  last_ip TEXT NOT NULL DEFAULT '',
  last_synced_at TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(fqdn)
);
