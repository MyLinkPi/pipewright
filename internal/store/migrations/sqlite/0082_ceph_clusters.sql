-- 0082 ceph:Ceph 集群部署与管理(cephadm)。
--
-- ceph_clusters  :集群登记(配置 + bootstrap 主机引用 + dashboard 凭据的 vault 引用;
--                  绝无明文口令;fsid/dashboard_url 由部署管线成功后回填)。
--   state        :created(已登记)→ deploying(部署中)→ ready(可用)/ failed(部署失败)。
--   cephadm_source:auto(发行版包优先,回退下载脚本)| package | curl:<脚本基础 URL>。
-- ceph_hosts     :主机意图(server_id 引用 servers;roles/labels 逗号分隔;devices 为
--                  OSD 块设备路径 JSON 数组;hostname/addr 由部署管线经 orch 回填)。
-- ceph_osds      :OSD 部署意图与回填(host 为 orch 主机名;osd_id 成功后回填,-1 = 未回填)。
-- Pool/RBD/CephFS 为经 bootstrap 主机 `ceph`/`rbd` CLI 的实时查询,不落库。
CREATE TABLE IF NOT EXISTS ceph_clusters (
    id                       TEXT PRIMARY KEY,
    name                     TEXT NOT NULL UNIQUE,
    fsid                     TEXT NOT NULL DEFAULT '',
    bootstrap_server_id      TEXT NOT NULL,
    image                    TEXT NOT NULL DEFAULT '',
    cephadm_source           TEXT NOT NULL DEFAULT 'auto',
    public_network           TEXT NOT NULL DEFAULT '',
    cluster_network          TEXT NOT NULL DEFAULT '',
    state                    TEXT NOT NULL DEFAULT 'created',
    dashboard_url            TEXT NOT NULL DEFAULT '',
    dashboard_user           TEXT NOT NULL DEFAULT '',
    dashboard_credential_id  TEXT NOT NULL DEFAULT '',
    created_at               TEXT NOT NULL,
    updated_at               TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS ceph_hosts (
    cluster_id TEXT NOT NULL,
    server_id  TEXT NOT NULL,
    roles      TEXT NOT NULL DEFAULT '',
    labels     TEXT NOT NULL DEFAULT '',
    devices    TEXT NOT NULL DEFAULT '[]',
    hostname   TEXT NOT NULL DEFAULT '',
    addr       TEXT NOT NULL DEFAULT '',
    added_at   TEXT NOT NULL,
    PRIMARY KEY (cluster_id, server_id)
);

CREATE INDEX IF NOT EXISTS idx_ceph_hosts_cluster ON ceph_hosts (cluster_id);

CREATE TABLE IF NOT EXISTS ceph_osds (
    cluster_id TEXT NOT NULL,
    host       TEXT NOT NULL,
    device     TEXT NOT NULL,
    osd_id     INTEGER NOT NULL DEFAULT -1,
    state      TEXT NOT NULL DEFAULT 'pending',
    created_at TEXT NOT NULL,
    PRIMARY KEY (cluster_id, host, device)
);

CREATE INDEX IF NOT EXISTS idx_ceph_osds_cluster ON ceph_osds (cluster_id);
