-- 0088 ssar_samples:ssar 机器监控的平台侧采样时序(机器监控页总览图表数据源)。
-- 后台采样器(env PIPEWRIGHT_SSAR_SAMPLE_INTERVAL,默认 60s;0 关闭)每轮对每台
-- 已装 ssar 的服务器跑一条合并 SSH 脚本(ssar --api 各视图取尾行)写入一行;
-- 机器侧细粒度历史(7 天)不入库,由按需查询直查。
--
-- 各列可空(NULL = 该指标该时刻不可得:视图段缺失 / 占位符 / 机器侧无数据)。
-- 口径:CPU 各项为 jiffies/s(全 CPU 汇总,未按核归一,与 ssar --cpu 一致);
-- 内存为 kB;load1/runq/threads 来自 load5s(5s 粒度);tcp_* 为瞬时连接数与每秒增量。
-- 时间用 pipewright 采样时刻(不采信 ssar 行内机器本地时间)。
-- server_id 不设外键:采样为历史快照,server 删除后旧趋势仍可查(由保留清理收敛)。
CREATE TABLE IF NOT EXISTS ssar_samples (
    id               TEXT PRIMARY KEY,
    server_id        TEXT NOT NULL,
    sampled_at       TEXT NOT NULL,
    cpu_user         REAL,
    cpu_system       REAL,
    cpu_iowait       REAL,
    cpu_idle         REAL,
    cpu_steal        REAL,
    mem_total_kb     REAL,
    mem_free_kb      REAL,
    mem_avail_kb     REAL,
    load1            REAL,
    runq             REAL,
    threads          REAL,
    tcp_curr_estab   REAL,
    tcp_retrans_segs REAL,
    tcp_syn_retrans  REAL,
    tcp_fast_retrans REAL,
    tcp_retrans_fail REAL
);

CREATE INDEX IF NOT EXISTS idx_ssar_samples_server_time ON ssar_samples (server_id, sampled_at);
CREATE INDEX IF NOT EXISTS idx_ssar_samples_time ON ssar_samples (sampled_at);
