// Package maintenance 是「磁盘维护」服务(磁盘治理,0094):把四类清理能力聚合成独立方法,
// 自动调度(retention Sweeper 任务 / 独立 ticker)与设置页手动按钮共用同一套实现。
//
//	 CleanBuildcache        构建依赖缓存(cache/):总量上限 LRU 淘汰 + .save-* 残留清理
//	 CleanRepocache         仓库镜像(repos/):删除无任何项目引用的镜像
//	 SweepRemoteWorkspaces  构建机(/tmp/pipewright-remote|shared):删除 48h 前的残留工作区
//	 SweepLocalTemp         本机 os.TempDir:删除 48h 前的构建孤儿临时文件
//
// 纪律:全部 best-effort——依赖未装配(缓存关闭等)返回 OK+说明;单条/单机失败跳过并计入
// Detail,绝不让清理动作本身报错中断;进行中的构建现场靠 mtime 年龄门天然不受影响。
package maintenance

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/huangchengsir/pipewright/internal/build"
	"github.com/huangchengsir/pipewright/internal/buildcache"
	"github.com/huangchengsir/pipewright/internal/repocache"
	"github.com/huangchengsir/pipewright/internal/retention"
	"github.com/huangchengsir/pipewright/internal/target"
)

// 年龄门:仅清 48h 前的残留。进行中构建的工作区/暂存 mtime 持续更新,天然不会命中;
// mesh 暂存 tar 的进程内 TTL(15min)正常路径先走,这里只兜崩溃/被杀的孤儿。
const orphanAge = 48 * time.Hour

// buildcacheTempMinAge 是 .save-* 半截文件的最小年龄(Save 在途的临时文件 mtime 新,不误删)。
const buildcacheTempMinAge = time.Hour

// 远程清扫命令(array 形态:sh -c 一条,内部 find 按年龄门删 /tmp 下两处构建目录的子项,
// 目录本身保留)。与既有远程命令同口径(remote_stage_exec 的 unpackCmd);2>/dev/null || true
// 保证目录不存在/个别子项删除失败都按成功收场(幂等,重复执行无害)。
var remoteSweepCmd = []string{"sh", "-c",
	`find /tmp/pipewright-remote /tmp/pipewright-shared -mindepth 1 -maxdepth 1 -mtime +2 -exec rm -rf {} + 2>/dev/null || true`}

// remoteSweepTimeout/remoteSweepConcurrency 是逐台远程清扫的单机超时与并发上限
//(SSH 逐台执行,大机房也要有界完成)。
const (
	remoteSweepTimeout     = 60 * time.Second
	remoteSweepConcurrency = 8
)

// Result 是一次清理的结果(手动入口的响应体;自动调度只记日志)。
type Result struct {
	Target string `json:"target"`
	OK     bool   `json:"ok"`
	Items  int    `json:"items"` // 删除条数(缓存条目/镜像数/文件数;远程清扫=机器数)
	Bytes  int64  `json:"bytes"` // 回收字节数(远程清扫恒 0——远端 find 不回报大小)
	Detail string `json:"detail,omitempty"` // 人读说明/跳过原因
	// Steps 是清理过程留痕:删了哪些文件/目录、本地执行了什么(上限 stepsMax 条,超出截断)。
	// 纪律对齐 build 的「所有执行的命令可见」:手动清理点了什么、动了什么,必须可查。
	Steps []string `json:"steps,omitempty"`
	// Machines 是远程清扫的逐机结果(仅 SweepRemoteWorkspaces 填;含每台实际执行的命令原文)。
	Machines []MachineResult `json:"machines,omitempty"`
}

// MachineResult 是单台构建机的清扫结果(含实际执行的命令原文,可审计)。
type MachineResult struct {
	ServerID string `json:"serverId"`
	Name     string `json:"name"`
	OK       bool   `json:"ok"`
	// Command 是在该机上实际执行的命令回显($ 前缀,与构建日志 emitCmd 同口径)。
	Command string `json:"command"`
	Error   string `json:"error,omitempty"`
}

// stepsMax 是 Steps 留痕的条数上限(防一次清几千个文件撑爆响应/日志;超出记"另有 N 项")。
const stepsMax = 100

// appendSteps 把删除路径清单收进 Steps(带动作前缀),超过上限截断并记尾注。
func appendSteps(steps []string, verb string, paths []string, extra ...string) []string {
	steps = append(steps, extra...)
	for _, p := range paths {
		if len(steps) >= stepsMax {
			return append(steps, fmt.Sprintf("…另有 %d 项未列出", len(paths)+len(extra)-len(steps)))
		}
		steps = append(steps, verb+" "+p)
	}
	return steps
}

// displayCommand 把 array 命令回显成单行(与构建日志 emitCmd 同口径:bin + 空格拼接参数;
// sh -c 的脚本体保持原样,不引号包裹——参数来自代码常量,无注入面)。
func displayCommand(cmd []string) string {
	if len(cmd) == 0 {
		return ""
	}
	return "$ " + strings.Join(cmd, " ")
}

// serverLister/remoteExecer 是远程清扫的最小依赖面(target.Service 原样满足;收窄接口便于
// 单测注入 fake,而不必伪造整个服务器领域)。
type serverLister interface {
	List(ctx context.Context) ([]*target.Server, error)
}

type remoteExecer interface {
	Exec(ctx context.Context, serverID string, cmd []string) (*target.ExecResult, error)
}

// Service 聚合四类清理依赖(均 nil 安全:未装配 = 该项跳过并如实说明)。
type Service struct {
	db        *sql.DB
	retention *retention.Service // 读 cache_max_gb 等磁盘治理配置(每轮现读,设置页可调)
	cache     *buildcache.Store  // nil = 构建缓存未启用(PIPEWRIGHT_NO_BUILD_CACHE)
	repos     *repocache.Cache   // nil = 仓库镜像未启用(PIPEWRIGHT_NO_REPO_CACHE)
	lister    serverLister       // 远程清扫的机器清单(nil = 跳过)
	execer    remoteExecer       // 远程清扫的 SSH 通道(nil = 跳过)
	tempDir   string             // 本机临时目录;空 = os.TempDir()
}

// New 构造维护服务(db 用于读 retention 配置与项目 repo_url 反查)。
func New(db *sql.DB) *Service {
	return &Service{db: db, retention: retention.NewService(db)}
}

// WithBuildCache 注入构建缓存库(链式;nil 安全)。
func (s *Service) WithBuildCache(c *buildcache.Store) *Service {
	s.cache = c
	return s
}

// WithRepoCache 注入仓库镜像缓存(链式;nil 安全)。
func (s *Service) WithRepoCache(c *repocache.Cache) *Service {
	s.repos = c
	return s
}

// WithTargets 注入目标服务器层(远程清扫的 SSH 通道与机器清单;链式)。
func (s *Service) WithTargets(t target.Service) *Service {
	if t != nil {
		s.lister, s.execer = t, t
	}
	return s
}

// WithTempDir 覆盖本机临时目录(默认 os.TempDir;链式)。
func (s *Service) WithTempDir(dir string) *Service {
	s.tempDir = dir
	return s
}

// CleanBuildcache 按配置上限(buildcache GC)+ 年龄门清理构建缓存。上限配置独立于 retention
// Enabled 开关——缓存不是审计数据;cache_max_gb=0 时仅清 .save-* 崩溃残留。
func (s *Service) CleanBuildcache(ctx context.Context) Result {
	res := Result{Target: "buildcache", OK: true}
	if s.cache == nil {
		res.Detail = "构建缓存未启用(PIPEWRIGHT_NO_BUILD_CACHE)"
		return res
	}
	maxGB := 0
	if s.retention != nil {
		if cfg, err := s.retention.GetConfig(ctx); err == nil {
			maxGB = cfg.CacheMaxGB
		} else {
			res.Detail = "读取缓存上限配置失败,仅清崩溃残留:" + err.Error()
		}
	}
	deleted, reclaimed, err := s.cache.GC(int64(maxGB)*(1<<30), buildcacheTempMinAge)
	res.Items, res.Bytes = len(deleted), reclaimed
	res.Steps = appendSteps(nil, "删除", deleted,
		fmt.Sprintf("缓存目录 %s:上限 %d GB(0=不限),按最久使用淘汰;另清超过 1h 的 .save-* 残留", s.cacheRoot(), maxGB))
	if err != nil {
		res.OK = false
		res.Detail = strings.TrimSpace(res.Detail + " " + err.Error())
	}
	return res
}

// cacheRoot 回显缓存根目录(留痕用;取不到返回 "?")。
func (s *Service) cacheRoot() string {
	if s.cache == nil {
		return "?"
	}
	return s.cache.Root()
}

// CleanRepocache 删除无任何项目引用的仓库镜像(定时兜底;项目删除的即时清理走 httpapi 侧)。
func (s *Service) CleanRepocache(ctx context.Context) Result {
	res := Result{Target: "repocache", OK: true}
	if s.repos == nil {
		res.Detail = "仓库镜像缓存未启用(PIPEWRIGHT_NO_REPO_CACHE)"
		return res
	}
	urls, err := s.projectRepoURLs(ctx)
	if err != nil {
		res.OK = false
		res.Detail = "查询项目仓库地址失败:" + err.Error()
		return res
	}
	deleted, reclaimed, err := s.repos.Prune(urls)
	res.Items, res.Bytes = len(deleted), reclaimed
	res.Steps = appendSteps(nil, "删除无引用镜像", deleted,
		fmt.Sprintf("镜像目录 %s:保留 %d 个仍在用的仓库地址,其余删除", s.repos.Root(), len(urls)))
	if err != nil {
		res.OK = false
		res.Detail = err.Error()
	}
	return res
}

// SweepRemoteWorkspaces 对全部登记构建机(Labels 非空的标准机,含共享构建机)执行 /tmp 残留
// 清扫。逐机独立成败、有界并发;SSH 不可达计入该机 error,不影响其它机器。
func (s *Service) SweepRemoteWorkspaces(ctx context.Context) Result {
	res := Result{Target: "remote-workspaces", OK: true}
	if s.lister == nil || s.execer == nil {
		res.Detail = "目标服务器层未装配"
		return res
	}
	servers, err := s.lister.List(ctx)
	if err != nil {
		res.OK = false
		res.Detail = "获取服务器清单失败:" + err.Error()
		return res
	}
	type job struct {
		id, name string
	}
	var jobs []job
	for _, sv := range servers {
		// 构建机池口径:Labels 非空才参与调度(runner 同款);PVE 物理机不参与。
		if sv == nil || sv.IsPVENode() || strings.TrimSpace(sv.Labels) == "" {
			continue
		}
		jobs = append(jobs, job{id: sv.ID, name: sv.Name})
	}
	if len(jobs) == 0 {
		res.Detail = "无登记构建机"
		return res
	}
	results := make([]MachineResult, len(jobs))
	sem := make(chan struct{}, remoteSweepConcurrency)
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = MachineResult{ServerID: j.id, Name: j.name, Command: displayCommand(remoteSweepCmd)}
			mctx, cancel := context.WithTimeout(ctx, remoteSweepTimeout)
			defer cancel()
			if _, err := s.execer.Exec(mctx, j.id, remoteSweepCmd); err != nil {
				results[i].Error = err.Error()
				return
			}
			results[i].OK = true
		}(i, j)
	}
	wg.Wait()
	ok := 0
	for _, m := range results {
		if m.OK {
			ok++
		}
	}
	res.Machines = results
	res.Items = ok
	if ok < len(results) {
		res.Detail = fmt.Sprintf("%d/%d 台成功", ok, len(results))
	}
	return res
}

// SweepLocalTemp 清扫本机 os.TempDir 下 48h 前的构建孤儿临时文件(进程被杀时 defer 未跑的
// 工作区/归档/mesh 暂存)。
func (s *Service) SweepLocalTemp(ctx context.Context) Result {
	_ = ctx // 清扫是本地文件系统操作,无取消点;保留参数与其它方法对齐
	dir := s.tempDir
	if dir == "" {
		dir = os.TempDir()
	}
	res := Result{Target: "local-temp", OK: true}
	deleted, reclaimed, err := build.SweepLocalOrphans(dir, orphanAge)
	res.Items, res.Bytes = len(deleted), reclaimed
	res.Steps = appendSteps(nil, "删除", deleted,
		fmt.Sprintf("扫描目录 %s:仅匹配构建前缀且超过 48h 的条目", dir))
	if err != nil {
		res.OK = false
		res.Detail = err.Error()
	}
	return res
}

// projectRepoURLs 返回全部项目的去重仓库地址(镜像保留集;维护包自查而非走 project.Service
// ——其 List 有分页上限,不适合全量反查)。
func (s *Service) projectRepoURLs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT repo_url FROM projects WHERE TRIM(repo_url) != ''`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var urls []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		urls = append(urls, u)
	}
	return urls, rows.Err()
}

// LogResult 把一次清理结果记进服务日志(自动调度入口用;手动入口由 HTTP 响应带回)。
// 明细逐行输出(命令回显/删除路径)——与构建日志「所有执行的命令可见」同口径。
func LogResult(r Result) {
	if r.OK {
		log.Printf("[maintenance] %s 完成:删除 %d 项,回收 %d 字节 %s", r.Target, r.Items, r.Bytes, r.Detail)
	} else {
		log.Printf("[maintenance] %s 失败:%s(已删除 %d 项)", r.Target, r.Detail, r.Items)
	}
	for _, m := range r.Machines {
		if m.OK {
			log.Printf("[maintenance] %s @%s(%s): %s", r.Target, m.Name, m.ServerID, m.Command)
		} else {
			log.Printf("[maintenance] %s @%s(%s): %s 失败:%s", r.Target, m.Name, m.ServerID, m.Command, m.Error)
		}
	}
	for _, st := range r.Steps {
		log.Printf("[maintenance] %s: %s", r.Target, st)
	}
}
