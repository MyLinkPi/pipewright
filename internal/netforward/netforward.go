// Package netforward 管控本机(pipewright 宿主)的 IPv4 转发开关,供跨环境服务转发的
// 数据面使用 —— 本机作为「网关机 ↔ EasyTier mesh」之间的三层转发点:
//
//		网关机 --/32 路由(下一跳=本机)--> 本机(ip_forward=1)--EasyTier 子网代理--> 对端 pipewright 宿主
//
//	  - Ensure:读 net.ipv4.ip_forward,为 0 时尝试写 1,并落 /etc/sysctl.d 持久化文件
//	    (重启后仍生效);无权限/非 Linux 不报错,只记录状态(界面告警,运维手工处理);
//	  - Probe:只读探测;
//	  - LastStatus:最近一次 Ensure/Probe 的快照(网关页/集群页展示)。
//
// 所有环境(对端)pipewright 宿主对称依赖此开关;本包只管本机一侧。
package netforward

import (
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	procForward = "/proc/sys/net/ipv4/ip_forward"
	persistDir  = "/etc/sysctl.d"
	persistFile = "/etc/sysctl.d/89-pipewright-ip-forward.conf"
	persistBody = "# managed by pipewright (mesh forward gateway)\nnet.ipv4.ip_forward = 1\n"
	// persistMarker 用于判断持久化文件是否本平台所写(存在但非本平台内容时重写)。
	persistMarker = "managed by pipewright"
)

// Status 是 ip_forward 保活/探测快照。
type Status struct {
	// OS 为 "linux" 或 "other"(非 Linux 上转发数据面不可用,仅展示提示)。
	OS string `json:"os"`
	// Enabled 表示 /proc/sys/net/ipv4/ip_forward 当前为 1。
	Enabled bool `json:"enabled"`
	// Persisted 表示 sysctl.d 持久化文件就位(重启后仍生效)。
	Persisted bool `json:"persisted"`
	// LastErr 是最近一次写入失败的文案(空 = 无)。
	LastErr string `json:"lastErr"`
	// CheckedAt 是最近一次探测时刻。
	CheckedAt time.Time `json:"checkedAt"`
}

var (
	mu   sync.Mutex
	last Status
)

// Probe 只读探测当前状态(不写任何系统配置)。
func Probe() Status {
	st := Status{OS: osName(), CheckedAt: time.Now().UTC()}
	if st.OS != "linux" {
		st.LastErr = "非 Linux 宿主,跨环境转发数据面不可用"
		save(st)
		return st
	}
	if v, err := os.ReadFile(procForward); err != nil {
		st.LastErr = "读取 ip_forward 失败: " + err.Error()
	} else {
		st.Enabled = strings.TrimSpace(string(v)) == "1"
	}
	if b, err := os.ReadFile(persistFile); err == nil && strings.Contains(string(b), persistMarker) {
		st.Persisted = true
	}
	save(st)
	return st
}

// Ensure 保活:开启 ip_forward(必要时)并持久化;失败仅记录进状态(不阻断调用方)。
func Ensure() Status {
	st := Probe()
	if st.OS != "linux" {
		return st
	}
	if strings.HasPrefix(st.LastErr, "读取") {
		return st // 连读都失败(如 /proc 未挂载),写无意义
	}
	if !st.Enabled {
		if err := os.WriteFile(procForward, []byte("1\n"), 0o644); err != nil {
			st.LastErr = "写入 ip_forward 失败(需 root): " + err.Error()
			save(st)
			return st
		}
		st.Enabled = true
	}
	if !st.Persisted {
		if err := os.MkdirAll(persistDir, 0o755); err != nil {
			st.LastErr = "创建 sysctl.d 失败(需 root): " + err.Error()
		} else if err := os.WriteFile(persistFile, []byte(persistBody), 0o644); err != nil {
			st.LastErr = "写入 sysctl.d 持久化失败(需 root): " + err.Error()
		} else {
			st.Persisted = true
		}
	}
	save(st)
	return st
}

// LastStatus 返回最近一次快照(尚未探测时现场补一次 Probe)。
func LastStatus() Status {
	mu.Lock()
	empty := last.CheckedAt.IsZero()
	mu.Unlock()
	if empty {
		return Probe()
	}
	return lastCopy()
}

func save(st Status) {
	mu.Lock()
	last = st
	mu.Unlock()
}

func lastCopy() Status {
	mu.Lock()
	defer mu.Unlock()
	return last
}

func osName() string {
	if runtime.GOOS == "linux" {
		return "linux"
	}
	return "other"
}
