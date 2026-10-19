// Package ssar 是「机器监控」域:ssar(自维护 fork MyLinkPi/ssar,上游 Anolis ssar)
// 的安装分发 + 指标采集 + 历史查询。
//
// 形态(agentless,与 6-1 服务器指标同一哲学,互不替代):
//   - 平台侧:Downloader 从 GitHub Release 拉安装包(rpm/deb,sha256 校验)缓存到
//     <data>/bin/ssar/<tag>/;UpgradeTrigger 异步单飞(仿 EasyTier 下载器 UX)。
//   - 机器侧:安装 = 上传安装包到 /tmp 经 rpm -Uvh --replacepkgs / dpkg -i 装入 —— 包自带
//     /usr/bin/{ssar,sresar}、/etc/ssar 配置与 systemd 服务(%post 自动 enable+start),
//     sresar 守护在机器本地 60s 采全量快照 + 5s 级负载,默认保留 7 天。安装需 root
//     (root 登录或免密 sudo,探测逻辑与 diskadmin 同款)。
//   - 采样:平台 ticker 对每台跑**一条**合并 SSH 脚本(##PW: 分段,各段
//     `ssar --<view> --api`),解析 JSON 尾行入 ssar_samples 时序表;机器侧细粒度历史
//     (7 天)由 Query 按需直查(白名单视图 → 固定 argv)。
//
// 解析契约(ssar 1.0.5,`--api`):单行 JSON 数组,元素为扁平对象,键 = 列别名 +
// collect_datetime,**值一律字符串**;"-"/"*" 等占位符 → 该值 null。时间口径:
// 入库时间用 pipewright 采样时刻,不采信 ssar 行内时间(机器本地时区);Query 原样
// 回传机器本地时间字符串,由前端标注。
//
// 安全纪律(AC-SEC-02 同源):
//   - 所有远端命令均为**纯静态 argv / 静态 sh -c 文本**,绝无用户输入拼接;Query 的
//     view/minutes/step 经白名单与数值区间校验后才进固定模板。
//   - 指标无敏感信息;错误体不含凭据明文。
//
// 容错纪律:
//   - 某台不可达 / 未装 ssar → 该台状态/采样失败,不连累其它台、不 500。
//   - 单个视图段缺失 / 解析失败 → 该组指标 null,不影响同轮其它视图。
//   - 非 Linux 目标 / aarch64(暂无发行资产)→ 明确人读错误。
package ssar

import (
	"context"
	"database/sql"
	"io"
	"sync"
	"time"

	"github.com/huangchengsir/pipewright/internal/target"
)

// ─── 对外 DTO(冻结契约)────────────────────────────────────────────────────

// Sample 是一台服务器某时刻的 ssar 采样(各字段 nil = 该指标不可得)。
// CPU 各项为 jiffies/s(全 CPU 汇总,未按核归一,与 ssar --cpu 口径一致);
// 内存为 kB(/proc/meminfo 口径);load1/runq/threads 来自 load5s(5s 粒度负载);
// currestab 为瞬时连接数,retranssegs 为每秒重传段数。
type Sample struct {
	ServerID string
	At       time.Time

	CPUUser   *float64
	CPUSystem *float64
	CPUIowait *float64
	CPUIdle   *float64
	CPUSteal  *float64

	MemTotalKB *float64
	MemFreeKB  *float64
	MemAvailKB *float64

	Load1   *float64
	Runq    *float64
	Threads *float64

	TCPCurrEstab   *float64
	TCPRetransSegs *float64
	TCPSynRetrans  *float64
	TCPFastRetrans *float64
	TCPRetransFail *float64
}

// Point 是时序查询返回的一个采样点(同 Sample,不含 ServerID;按时间升序)。
type Point struct {
	At time.Time `json:"at"`

	CPUUser   *float64 `json:"cpuUser"`
	CPUSystem *float64 `json:"cpuSystem"`
	CPUIowait *float64 `json:"cpuIowait"`
	CPUIdle   *float64 `json:"cpuIdle"`
	CPUSteal  *float64 `json:"cpuSteal"`

	MemTotalKB *float64 `json:"memTotalKb"`
	MemFreeKB  *float64 `json:"memFreeKb"`
	MemAvailKB *float64 `json:"memAvailKb"`

	Load1   *float64 `json:"load1"`
	Runq    *float64 `json:"runq"`
	Threads *float64 `json:"threads"`

	TCPCurrEstab   *float64 `json:"tcpCurrEstab"`
	TCPRetransSegs *float64 `json:"tcpRetransSegs"`
	TCPSynRetrans  *float64 `json:"tcpSynRetrans"`
	TCPFastRetrans *float64 `json:"tcpFastRetrans"`
	TCPRetransFail *float64 `json:"tcpRetransFail"`
}

// Status 是单台服务器的 ssar 安装/采集器状态(探测所得,经 TTL 缓存)。
type Status struct {
	ServerID string
	// ProbeOK=false 表示探测本身失败(SSH 不通 / 非 Linux),Error 为人读原因。
	ProbeOK bool
	// Installed 表示机器上能执行 ssar(VERSION 已解析;空 = 未能解析)。
	Installed bool
	Version   string
	// CollectorActive 表示 systemctl is-active sresar == active。
	CollectorActive bool
	// OS/Arch 为探测到的 uname 结果(Linux/x86_64…;探测失败为空)。
	OS   string
	Arch string
	// Error 为人读错误(探测失败时非空)。
	Error string
}

// InstallResult 是一次安装(单台)的结果。
type InstallResult struct {
	ServerID string `json:"serverId"`
	OK       bool   `json:"ok"`
	Version  string `json:"version"` // 成功后解析到的 ssar 版本(如 "1.0.5")
	Error    string `json:"error"`   // 失败时的人读原因(绝无凭据明文)
}

// QueryColumn 是按需查询结果的一列(键 = ssar 别名;Unit 来自静态口径表,未知为空)。
type QueryColumn struct {
	Key  string `json:"key"`
	Unit string `json:"unit"`
}

// QueryRow 是按需查询结果的一行(Time 为机器本地时间原样字符串;Values 值为
// float64(可解析数值)或 string(如 stype/sstate),占位符键不存在)。
type QueryRow struct {
	Time   string         `json:"time"`
	Values map[string]any `json:"values"`
}

// QueryResult 是一次按需视图查询的结果(行按时间升序)。
type QueryResult struct {
	View    string        `json:"view"`
	Columns []QueryColumn `json:"columns"`
	Rows    []QueryRow    `json:"rows"`
}

// Service 定义机器监控域对外接口(httpapi / main 采样器消费)。
type Service interface {
	// BinStatus 返回平台侧安装包缓存状态(版本/已缓存包名/下载中/最近错误)。
	BinStatus() BinStatus
	// TriggerDownload 异步触发一次安装包下载(单飞:进行中重复触发为 no-op)。
	TriggerDownload(ctx context.Context)

	// Install 把缓存的 ssar 安装包部署到 serverIDs 各机(有界并发;逐台独立成败)。
	Install(ctx context.Context, serverIDs []string) []InstallResult
	// Status 探测单台状态(经 TTL 缓存;探测失败不缓存成功态)。
	Status(ctx context.Context, serverID string) (Status, error)

	// SampleAll 对全部登记服务器各跑一轮合并采集并入库,返回入库样本数。
	// 未装 ssar / 不可达 / 全字段缺失的机器自然跳过(不报错)。
	SampleAll(ctx context.Context) (int, error)
	// Query 按需查询机器侧 ssar 历史(view 白名单;minutes ≤ 10080;step 分钟 ≥ 1,
	// 且 minutes/step ≤ 1440 防大结果)。
	Query(ctx context.Context, serverID, view string, minutes, stepMinutes int) (*QueryResult, error)

	// History 返回平台采样时序(自 since 起,升序;供总览图表)。
	History(ctx context.Context, serverID string, since time.Time) ([]Point, error)
	// LastSampleAt 返回该台最近一次入库采样时间(无样本返回零值)。
	LastSampleAt(ctx context.Context, serverID string) (time.Time, error)
	// Sweep 删除早于 before 的样本(保留清理),返回删除行数。
	Sweep(ctx context.Context, before time.Time) (int64, error)
}

// ─── Runner 抽象与生产实现 ────────────────────────────────────────────────────

// Runner 抽象「对一台登记服务器执行命令 / 上传文件」(注入 fake 可离线单测)。
// Run 提权执行(安装用:root 登录直跑,否则免密 sudo -n,探测缓存 10 分钟);
// RunPlain 不提权(探测与 ssar 查询用:ssar 读全可读的数据文件,无需 root)。
type Runner interface {
	Run(ctx context.Context, serverID string, cmd []string) (*target.ExecResult, error)
	RunPlain(ctx context.Context, serverID string, cmd []string) (*target.ExecResult, error)
	Upload(ctx context.Context, serverID string, content io.Reader, remotePath string) error
}

// service 是 store + runner + downloader 支撑的 Service 实现。
type service struct {
	db         *sql.DB
	servers    target.Service
	runner     Runner
	downloader *Downloader

	// 状态探测 TTL 缓存(serverID → entry);仿 server_metrics 快照思路,防页面轮询
	// 把探测 SSH 打爆。
	statusMu    sync.Mutex
	statusCache map[string]statusEntry
}

// Option 配置 New(测试注入 fake Runner 等)。
type Option func(*service)

// WithRunner 覆盖 SSH 执行器(单测注入 fake,不触网)。
func WithRunner(r Runner) Option { return func(s *service) { s.runner = r } }

// New 构造 Service。downloader 可为 nil(则 BinStatus 报未配置、Install 报请先下载)。
func New(db *sql.DB, servers target.Service, downloader *Downloader, opts ...Option) Service {
	s := &service{
		db:          db,
		servers:     servers,
		runner:      newSSHRunner(servers),
		downloader:  downloader,
		statusCache: map[string]statusEntry{},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// statusEntry 是状态探测缓存条目(status 为探测结果;probeFailed 时不入长缓存)。
type statusEntry struct {
	status Status
	at     time.Time
}
