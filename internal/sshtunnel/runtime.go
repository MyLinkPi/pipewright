package sshtunnel

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// helper 协议常量:主进程与 helper 子进程经 <dataDir>/tunnels/ 下的文件协作。
//
//	state/<id>.json : 心跳+状态(helper 每 5s 原子替换;主进程读,新鲜 = helper 存活)
//	state/<id>.stop : 停止旗标(主进程写;helper ≤1s 内见到即优雅退出并清理两文件)
//	logs/<id>.log   : helper 日志(每次拉起截断,上限 1MB;含连接/重连/错误事件,无密钥)
const (
	// HelperFlag 是 helper 模式的 argv 首参(cmd/pipewright/main.go 据此在正常装配前
	// 分派到 sshtunnel.RunHelperMain)。
	HelperFlag = "--tunnel-helper"
	stateSuffix = ".json"
	stopSuffix  = ".stop"
	logSuffix   = ".log"
	logMaxBytes = 1 << 20
	// stopWait 是停止旗标后等 helper 优雅退出的窗口,超时按状态文件 pid 强杀。
	stopWait = 8 * time.Second
)

// helper 汇报的运行状态(StateFile.Status)。
const (
	StatusConnecting = "connecting" // 建连/建转发中
	StatusActive     = "active"     // 转发面已就绪
	StatusRetrying   = "retrying"   // 断线/失败后按退避重连中(LastError 带原因)
)

// 主进程侧合并出的展示状态(TunnelView.Status)。
const (
	StatusDisabled = "disabled" // 停用(enabled = false)
	StatusStopped  = "stopped"  // 已启用但 helper 不在跑(对账循环将自动拉起)
)

// StateFile 是 helper 心跳文件形态(<dataDir>/tunnels/state/<id>.json)。
// 时间为 RFC3339 字符串(原子写:临时文件 + rename)。
type StateFile struct {
	PID         int    `json:"pid"`
	Status      string `json:"status"`
	LastError   string `json:"lastError"`
	StartedAt   string `json:"startedAt"`
	UpdatedAt   string `json:"updatedAt"`
	Restarts    int    `json:"restarts"`
	ActualPort  int    `json:"actualPort"`
	Endpoint    string `json:"endpoint"`
	Fingerprint string `json:"fingerprint"`
}

// Live 报告心跳是否新鲜(连续 4 拍未更新视为 helper 已死)。
func (s *StateFile) Live() bool {
	updated, err := time.Parse(time.RFC3339, s.UpdatedAt)
	if err != nil {
		return false
	}
	return time.Since(updated) < staleAfter
}

// StartedAtTime 解析启动时刻(损坏时返回零值)。
func (s *StateFile) StartedAtTime() time.Time {
	t, _ := time.Parse(time.RFC3339, s.StartedAt)
	return t
}

// TunnelView 是配置 + 运行态的合并视图(HTTP 层转 DTO)。
type TunnelView struct {
	Tunnel
	Status        string `json:"status"`
	RuntimeStatus string `json:"-"`
	PID           int    `json:"pid"`
	LastError     string `json:"lastError"`
	Restarts      int    `json:"restarts"`
	ActualPort    int    `json:"actualPort"`
	Endpoint      string `json:"endpoint"`
	StartedAt     time.Time `json:"startedAt"`
}

// Runtime 是 helper 子进程运行时(拉起/停止/状态读取)。宿主停机**不杀** helper
//(mesh easytier 同款:停机即孤儿,下次启动心跳新鲜即零中断收养),因此这里没有
// 「随 ctx 关停」的路径——Stop 仅由停用/删除/配置变更/重启显式调用。
type Runtime struct {
	stateDir string
	logDir   string

	mu       sync.Mutex
	failMsgs map[string]string // id → 已记日志的拉起失败(同 id 不逐拍刷屏)

	// spawnFn/stopFn 是真实实现的可注入缝(单测替换,绝不真实拉起子进程)。
	spawnFn func(id string) error
	stopFn  func(id string)
}

// NewRuntime 构造 Runtime 并确保目录就位。
func NewRuntime(dataDir string) *Runtime {
	rt := &Runtime{
		stateDir: filepath.Join(dataDir, "tunnels", "state"),
		logDir:   filepath.Join(dataDir, "tunnels", "logs"),
		failMsgs: map[string]string{},
	}
	rt.spawnFn = rt.spawnProcess
	rt.stopFn = rt.stopProcess
	_ = os.MkdirAll(rt.stateDir, 0o755)
	_ = os.MkdirAll(rt.logDir, 0o755)
	return rt
}

// Spawn 拉起 helper(默认真实子进程;单测注入替换)。
func (r *Runtime) Spawn(id string) error { return r.spawnFn(id) }

// Stop 停止 helper(默认走旗标协议;单测注入替换)。
func (r *Runtime) Stop(id string) { r.stopFn(id) }

func (r *Runtime) statePath(id string) string { return filepath.Join(r.stateDir, id+stateSuffix) }
func (r *Runtime) stopPath(id string) string  { return filepath.Join(r.stateDir, id+stopSuffix) }
func (r *Runtime) logPath(id string) string   { return filepath.Join(r.logDir, id+logSuffix) }

// State 读取并解析心跳文件;缺失/损坏返回 nil(调用方据 Live 判存活)。
func (r *Runtime) State(id string) *StateFile {
	data, err := os.ReadFile(r.statePath(id))
	if err != nil {
		return nil
	}
	var st StateFile
	if err := json.Unmarshal(data, &st); err != nil {
		return nil
	}
	return &st
}

// WriteState 原子写心跳(临时文件 + rename;helper 进程内使用,主进程不写)。
func (r *Runtime) WriteState(id string, st *StateFile) error {
	st.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := r.statePath(id) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.statePath(id))
}

// spawnProcess 拉起 helper 子进程(argv 仅隧道 id,绝无密钥;env 继承主进程,含
// master key / DB 配置,helper 自行解析)。stdio 重定向到该隧道的日志文件
//(截断重开,上限 1MB);立即返回,连接与重连由 helper 自治。
func (r *Runtime) spawnProcess(id string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("sshtunnel: locate executable: %w", err)
	}
	// 清残留停止旗标(上次 Stop 在 helper 未跑时立下的),防新 helper 起跑即被旧旗标击杀。
	_ = os.Remove(r.stopPath(id))
	logFile, err := os.OpenFile(r.logPath(id), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("sshtunnel: open tunnel log: %w", err)
	}
	capped := &cappedWriter{w: logFile, max: logMaxBytes}
	cmd := exec.Command(exe, HelperFlag, "--id", id)
	cmd.Stdout = capped
	cmd.Stderr = capped
	cmd.SysProcAttr = detachedSysProcAttr()
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("sshtunnel: spawn helper: %w", err)
	}
	// 宿主存活期间收尸防僵尸;宿主退出后 helper 由 init 接管(孤儿化,不受影响)。
	go func() {
		_ = cmd.Wait()
		_ = logFile.Close()
	}()
	r.clearSpawnFail(id)
	return nil
}

// ShouldLogSpawnFail 对账循环的失败去重:同一 id 错误文本不变时只记一次
//(mesh failedKey 先例),变化(如二进制修复)再记。
func (r *Runtime) ShouldLogSpawnFail(id, msg string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failMsgs[id] == msg {
		return false
	}
	r.failMsgs[id] = msg
	return true
}

// clearSpawnFail 清除拉起失败记录(Spawn 成功时调用)。
func (r *Runtime) clearSpawnFail(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.failMsgs, id)
}

// stopProcess 停止 helper:先立停止旗标(挡住一切形态的 helper —— 已跑的守望循环
// ≤1s 看到;刚拉起尚未写首拍心跳的,启动检查也会看到即退),再等状态文件消失
//(= helper 已优雅退出并自清两文件);超时按心跳 pid 强杀并清残留。
func (r *Runtime) stopProcess(id string) {
	// 1) 先立旗标再判定:防「helper 刚 spawn、心跳还没落地」的竞争窗口。
	if err := os.WriteFile(r.stopPath(id), []byte{}, 0o600); err != nil {
		// 旗标写不进(磁盘满/权限):退化为按 pid 强杀,保证停得住。
		if st := r.State(id); st != nil {
			r.killByState(st)
		}
		r.CleanupState(id)
		return
	}
	// 2) 等优雅退出:状态文件消失即成(helper 退出时一并清旗标)。
	deadline := time.Now().Add(stopWait)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(r.statePath(id)); os.IsNotExist(err) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	// 3) 超时(helper 卡死在不可中断调用上):按心跳 pid 强杀 + 清残留。
	//    心跳缺失时只清文件 —— 下拍对账按配置期望重拉,不误杀无关进程。
	if st := r.State(id); st != nil {
		r.killByState(st)
	}
	r.CleanupState(id)
}

func (r *Runtime) killByState(st *StateFile) {
	if st.PID <= 0 {
		return
	}
	if p, err := os.FindProcess(st.PID); err == nil {
		_ = p.Kill()
	}
}

// CleanupState 删除心跳与旗标文件(状态文件已陈旧时由对账循环回收)。
func (r *Runtime) CleanupState(id string) {
	_ = os.Remove(r.statePath(id))
	_ = os.Remove(r.stopPath(id))
}

// Cleanup 彻底清理该隧道的全部运行时文件(删除隧道配置时调用)。
func (r *Runtime) Cleanup(id string) {
	r.CleanupState(id)
	_ = os.Remove(r.logPath(id))
}

// TailLog 返回日志尾部 lines 行;文件缺失(从未运行)返回空。
func (r *Runtime) TailLog(id string, lines int) []string {
	data, err := os.ReadFile(r.logPath(id))
	if err != nil {
		return []string{}
	}
	text := strings.TrimRight(string(data), "\r\n")
	if text == "" {
		return []string{}
	}
	all := strings.Split(text, "\n")
	for i, line := range all {
		all[i] = strings.TrimRight(line, "\r")
	}
	if len(all) <= lines {
		return all
	}
	return all[len(all)-lines:]
}

// cappedWriter 把写入封顶在 max 字节(超限静默丢弃;helper 日志只含事件行,
// 1MB 上限防异常刷屏撑爆磁盘)。
type cappedWriter struct {
	w   io.Writer
	n   int64
	max int64
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if c.n >= c.max {
		return len(p), nil
	}
	if c.n+int64(len(p)) > c.max {
		p = p[:c.max-c.n]
	}
	n, err := c.w.Write(p)
	c.n += int64(n)
	return len(p), err // 上报原始长度,调用方(exec)不因截断而中断
}
