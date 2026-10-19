-- 0088 ssar_samples(MySQL):ssar 机器监控的平台侧采样时序(机器监控页总览图表数据源)。
-- 各列可空(NULL = 该指标该时刻不可得);时间用 pipewright 采样时刻;按保留窗口定期清理。
-- 口径同 sqlite 孪生:CPU jiffies/s、内存 kB、load5s 负载、tcp 瞬时/每秒增量。
CREATE TABLE IF NOT EXISTS ssar_samples (
    id               VARCHAR(64) PRIMARY KEY,
    server_id        VARCHAR(64) NOT NULL,
    sampled_at       VARCHAR(32) NOT NULL,
    cpu_user         DOUBLE NULL,
    cpu_system       DOUBLE NULL,
    cpu_iowait       DOUBLE NULL,
    cpu_idle         DOUBLE NULL,
    cpu_steal        DOUBLE NULL,
    mem_total_kb     DOUBLE NULL,
    mem_free_kb      DOUBLE NULL,
    mem_avail_kb     DOUBLE NULL,
    load1            DOUBLE NULL,
    runq             DOUBLE NULL,
    threads          DOUBLE NULL,
    tcp_curr_estab   DOUBLE NULL,
    tcp_retrans_segs DOUBLE NULL,
    tcp_syn_retrans  DOUBLE NULL,
    tcp_fast_retrans DOUBLE NULL,
    tcp_retrans_fail DOUBLE NULL,
    INDEX idx_ssar_samples_server_time (server_id, sampled_at),
    INDEX idx_ssar_samples_time (sampled_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
