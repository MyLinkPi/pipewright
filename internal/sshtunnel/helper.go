package sshtunnel

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/huangchengsir/pipewright/internal/config"
	"github.com/huangchengsir/pipewright/internal/store"
	"github.com/huangchengsir/pipewright/internal/target"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// helper 重连退避参数(同 mesh supervisor:2s 起、翻倍、30s 封顶,成功后重置)。
const (
	retryBackoffStart = 2 * time.Second
	retryBackoffMax   = 30 * time.Second
)

// RunHelperMain 是 --tunnel-helper 模式的进程入口(cmd/pipewright/main.go 在任何
// 正常装配之前分派到此)。argv 只有隧道 id(绝无密钥);DB/vault 配置经继承的 env
// 自行解析(与主进程同一 config.Load),凭据在 helper 进程内即用即弃。
//
// 退出码:0 = 被停止旗标正常终止;1 = 配置/环境错误(隧道不存在、保险库未配置等,
// 由主进程的对账循环按 enabled 持续重试拉起,错误见该隧道的日志文件)。
func RunHelperMain(args []string) error {
	id := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--id" && i+1 < len(args) {
			id = args[i+1]
			i++
		}
	}
	if id == "" {
		return errors.New("sshtunnel helper: missing --id")
	}

	cfg := config.Load()
	dbDriver, dbDSN, err := cfg.StoreConfig()
	if err != nil {
		return fmt.Errorf("sshtunnel helper: config: %w", err)
	}
	st, err := store.OpenWithConfig(store.OpenConfig{Driver: dbDriver, DSN: dbDSN})
	if err != nil {
		return fmt.Errorf("sshtunnel helper: store: %w", err)
	}
	defer func() { _ = st.Close() }()

	masterKey, keyErr := config.LoadMasterKey()
	if keyErr != nil {
		// 与主进程同一语义:master key 缺失/无效 → 凭据不可取,helper 无法工作。
		return fmt.Errorf("sshtunnel helper: vault master key unavailable: %w", keyErr)
	}
	targets := target.New(st.DB, vault.New(st.DB, masterKey), nil)

	tun, err := LoadByID(context.Background(), st.DB, id)
	if err != nil {
		return fmt.Errorf("sshtunnel helper: load tunnel %s: %w", id, err)
	}

	// 数据目录与主进程同一解析:SQLite 库文件同级(MySQL 部署沿用 cwd 约定)。
	dataDir := filepath.Dir(cfg.DBPath)
	return runHelper(tun, targets, dataDir)
}

// runHelper 是 helper 主循环:建连 → 起转发 → 活性守望(心跳 + keepalive)→
// 断线退避重连;任何时刻响应停止旗标(≤1s)优雅退出并清理状态文件。
func runHelper(t *Tunnel, targets target.Service, dataDir string) error {
	rt := NewRuntime(dataDir)
	// 日志走 stderr:由主进程的 stdio 重定向落到该隧道的日志文件(带 1MB 封顶);
	// 手工在前台跑 helper 时则直接打到控制台。
	logger := log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds)

	// 开跑前先看停止旗标(主进程 stop→spawn 竞争:旗标已在则直接退,不抢跑)。
	if stopRequested(rt, t.ID) {
		rt.CleanupState(t.ID)
		return nil
	}

	state := &StateFile{
		PID:         os.Getpid(),
		Status:      StatusConnecting,
		StartedAt:   time.Now().UTC().Format(time.RFC3339),
		Fingerprint: t.Fingerprint(),
	}
	if err := rt.WriteState(t.ID, state); err != nil {
		logger.Printf("[tunnel] 警告:心跳写入失败(宿主将视为未运行):%v", err)
	}
	logger.Printf("[tunnel] %s(mode=%s)启动:bind=%s:%d", t.Name, t.Mode, t.BindHost, t.BindPort)

	backoff := retryBackoffStart
	dialAttempts := 0
	for {
		if stopRequested(rt, t.ID) {
			cleanupExit(rt, t.ID, logger)
			return nil
		}
		if state.Status != StatusConnecting {
			state.Status = StatusConnecting
			_ = rt.WriteState(t.ID, state)
		}

		client, err := DialTunnel(context.Background(), t, targets)
		if err != nil {
			dialAttempts++
			if dialAttempts > 1 {
				state.Restarts++
			}
			state.Status = StatusRetrying
			state.LastError = humanError(err)
			_ = rt.WriteState(t.ID, state)
			logger.Printf("[tunnel] 连接失败:%s,%v 后重试", state.LastError, backoff)
			if !sleepInterruptable(rt, t.ID, backoff) {
				cleanupExit(rt, t.ID, logger)
				return nil
			}
			backoff = min(backoff*2, retryBackoffMax)
			continue
		}
		backoff = retryBackoffStart // 建连成功即重置退避

		fw, err := startForward(client, t)
		if err != nil {
			_ = client.Close()
			dialAttempts++
			state.Restarts++
			state.Status = StatusRetrying
			state.LastError = humanListenError(err)
			_ = rt.WriteState(t.ID, state)
			logger.Printf("[tunnel] 转发建立失败:%s,%v 后重试", state.LastError, backoff)
			if !sleepInterruptable(rt, t.ID, backoff) {
				cleanupExit(rt, t.ID, logger)
				return nil
			}
			backoff = min(backoff*2, retryBackoffMax)
			continue
		}

		state.Status = StatusActive
		state.LastError = ""
		state.ActualPort = fw.actualPort
		state.Endpoint = fw.endpoint
		_ = rt.WriteState(t.ID, state)
		logger.Printf("[tunnel] 转发就绪:%s(实际端口 %d)", fw.endpoint, fw.actualPort)

		// 活性守望:每秒查停止旗标,每心跳拍(5s)keepalive 探活 + 写心跳。
		// keepalive 失败/超时 = 连接已死(TCP 半开检测),关闭转发面后重连。
		if !superviseActive(rt, t.ID, client, fw, state, logger) {
			cleanupExit(rt, t.ID, logger)
			return nil
		}
		state.Restarts++
		state.Status = StatusRetrying
		state.LastError = "连接断开,正在重连"
		_ = rt.WriteState(t.ID, state)
		logger.Printf("[tunnel] 连接断开:%v 后重连", backoff)
		if !sleepInterruptable(rt, t.ID, backoff) {
			cleanupExit(rt, t.ID, logger)
			return nil
		}
		backoff = min(backoff*2, retryBackoffMax)
	}
}

// superviseActive 守望已就绪的转发面,直至连接死亡或收到停止旗标。
// 返回 false = 收到停止旗标(调用方清理退出);true = 连接已死(调用方走重连)。
func superviseActive(rt *Runtime, id string, client target.SSHClient, fw *forward, state *StateFile, logger *log.Logger) bool {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var heartbeat time.Time
	for {
		select {
		case <-tick.C:
			if stopRequested(rt, id) {
				fw.close()
				_ = client.Close()
				return false
			}
			if time.Since(heartbeat) < heartbeatInterval {
				continue
			}
			heartbeat = time.Now()
			// 先写心跳(宿主可见「仍在运行」),再探活;探活失败下一拍进入重连。
			_ = rt.WriteState(id, state)
			if err := client.Ping(heartbeatInterval); err != nil {
				logger.Printf("[tunnel] keepalive 探活失败:%v", err)
				fw.close()
				_ = client.Close()
				return true
			}
		}
	}
}

// stopRequested 报告停止旗标是否已置位(主进程写 <id>.stop)。
func stopRequested(rt *Runtime, id string) bool {
	_, err := os.Stat(rt.stopPath(id))
	return err == nil
}

// sleepInterruptable 睡眠 d,期间每 500ms 检查停止旗标;返回 false = 收到停止。
func sleepInterruptable(rt *Runtime, id string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		if stopRequested(rt, id) {
			return false
		}
	}
	return true
}

// cleanupExit 优雅退出收尾:清状态与旗标文件(helper 对「我退出了」的唯一声明)。
func cleanupExit(rt *Runtime, id string, logger *log.Logger) {
	rt.CleanupState(id)
	logger.Printf("[tunnel] 收到停止指令,已退出")
}

// min 返回两时长中较小者。
func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
