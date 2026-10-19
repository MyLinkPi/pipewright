// Package pwclient 是 pipewright 集群客户端(Win11 pwclient.exe)的核心领域层:GUI 无关、
// 可单测。职责:
//   - 配置(YAML)持久化与归一化:加入参数(网络名/密钥/peers/虚拟 IP)与路由网段列表;
//   - 运行时:复用 internal/mesh/easytier 的监督器托管 easytier-core 子进程组网,
//     指纹对账(配置变化才重启),二进制定位 + 缺失自动下载;
//   - IPC:Windows 服务(数据面)与托盘 GUI(管理面)之间的 loopback HTTP + token 通道。
//
// 数据面说明:本节点(客户端)不通告任何子网(ProxyNetworks 恒空);routes 非空时经
// easytier --manual-routes 限定本机路由哪些「服务端通告的子网代理网段」(空 = 全部)。
// 远端网段的出口 NAT 由通告该网段的服务端节点承担(EasyTier 子网代理内置用户态 NAT,
// 被代理网段内设备无需回程路由)。
package pwclient

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ErrInvalidConfig 表示配置非法(CIDR/IP 格式错误等);错误体不含密钥。
var ErrInvalidConfig = errors.New("pwclient: invalid config")

// JoinConfig 是加入集群的组网参数(与平台「导出客户端配置」文件的 join 段对齐)。
type JoinConfig struct {
	NetworkName   string   `yaml:"networkName"`
	NetworkSecret string   `yaml:"networkSecret"` // 明文落盘(0600);绝不入日志/状态回显
	Peers         []string `yaml:"peers"`         // 至少一个公网可达的对端地址(如 tcp://IP:11010)
	VirtualIP     string   `yaml:"virtualIp"`     // 空 = DHCP 自动分配
	Hostname      string   `yaml:"hostname"`      // 空 = 计算机名
}

// EasyTierConfig 是 easytier 二进制定位与本地 RPC 状态口。
type EasyTierConfig struct {
	BinDir  string `yaml:"binDir"`  // 空 = <dataDir>/bin
	RPCAddr string `yaml:"rpcAddr"` // 空 = 127.0.0.1:15889(避开平台节点默认的 15888)
}

// Config 是客户端全部配置(config.yaml;服务与 GUI 共享读写)。
type Config struct {
	// Enabled 是连接开关(托盘「断开/连接」);false = 停止组网、保留配置。
	Enabled bool       `yaml:"enabled"`
	Join    JoinConfig `yaml:"join"`
	// Routes 手工限定本机路由的网段(cidr)。空 = 自动接受全部子网代理通告;
	// 非空 = 经 --manual-routes 仅路由列表内网段。
	Routes   []string       `yaml:"routes"`
	EasyTier EasyTierConfig `yaml:"easytier"`
}

// exportMeta 是平台导出文件中客户端不消费的辅助段(来源节点/平台版本,留档展示)。
type exportMeta struct {
	From            string `yaml:"from"`
	NodeID          string `yaml:"nodeId"`
	PlatformVersion string `yaml:"platformVersion"`
}

// exportFile 是平台「导出客户端配置」产出的 pipewright-client.yaml 结构。
type exportFile struct {
	Join   JoinConfig `yaml:"join"`
	Routes []string   `yaml:"routes"`
	Export exportMeta `yaml:"export"`
}

// Store 是配置文件的加载/保存(原子写;读写均经 Normalize 归一)。
type Store struct {
	Path string
}

// Load 读取配置;文件不存在返回零值(未配置态)而非错误。
func (s *Store) Load() (Config, error) {
	raw, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("pwclient: read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	return Normalize(cfg)
}

// Save 归一化后原子落盘(0600:文件含明文组网密钥,仅本机管理员/服务可读)。
func (s *Store) Save(cfg Config) error {
	cfg, err := Normalize(cfg)
	if err != nil {
		return err
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("pwclient: marshal config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return fmt.Errorf("pwclient: mkdir config dir: %w", err)
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return fmt.Errorf("pwclient: write config: %w", err)
	}
	return os.Rename(tmp, s.Path)
}

// Normalize 归一化:去空白/去空项/去重(保序)、虚拟 IP 合法性、routes 逐项须为合法
// CIDR(非法带原值报错 —— 配置来自管理员/GUI 手输,须即时反馈)、easytier 默认值回填。
// 密钥仅去空白,不校验内容。
func Normalize(cfg Config) (Config, error) {
	cfg.Join.NetworkName = strings.TrimSpace(cfg.Join.NetworkName)
	cfg.Join.NetworkSecret = strings.TrimSpace(cfg.Join.NetworkSecret)
	cfg.Join.VirtualIP = strings.TrimSpace(cfg.Join.VirtualIP)
	cfg.Join.Hostname = strings.TrimSpace(cfg.Join.Hostname)
	cfg.Join.Peers = dedupeTrim(cfg.Join.Peers)
	cfg.EasyTier.BinDir = strings.TrimSpace(cfg.EasyTier.BinDir)
	cfg.EasyTier.RPCAddr = strings.TrimSpace(cfg.EasyTier.RPCAddr)
	if cfg.EasyTier.RPCAddr == "" {
		cfg.EasyTier.RPCAddr = "127.0.0.1:15889"
	}
	if cfg.Join.VirtualIP != "" && net.ParseIP(cfg.Join.VirtualIP) == nil {
		return cfg, fmt.Errorf("%w: 虚拟 IP %q 不是合法地址", ErrInvalidConfig, cfg.Join.VirtualIP)
	}
	routes := make([]string, 0, len(cfg.Routes))
	seen := make(map[string]struct{}, len(cfg.Routes))
	for _, r := range cfg.Routes {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if _, _, err := net.ParseCIDR(r); err != nil {
			return cfg, fmt.Errorf("%w: %q 不是合法的 CIDR 网段(形如 192.168.30.0/24)", ErrInvalidConfig, r)
		}
		if _, dup := seen[r]; dup {
			continue
		}
		seen[r] = struct{}{}
		routes = append(routes, r)
	}
	cfg.Routes = routes
	return cfg, nil
}

// ImportExport 解析平台导出的 pipewright-client.yaml,产出可合并的加入参数与预填路由。
// 仅返回 join 与 routes 两个可合并段;本机既有配置的 enabled/easytier 段由调用方保留。
func ImportExport(raw []byte) (join JoinConfig, routes []string, meta exportMeta, err error) {
	var f exportFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return join, nil, meta, fmt.Errorf("%w: 不是合法的导出文件: %v", ErrInvalidConfig, err)
	}
	if strings.TrimSpace(f.Join.NetworkName) == "" || strings.TrimSpace(f.Join.NetworkSecret) == "" {
		return join, nil, meta, fmt.Errorf("%w: 导出文件缺少网络名或组网密钥", ErrInvalidConfig)
	}
	cfg, err := Normalize(Config{Join: f.Join, Routes: f.Routes})
	if err != nil {
		return join, nil, meta, err
	}
	return cfg.Join, cfg.Routes, f.Export, nil
}

// Configured 报告加入参数是否已配置完整(网络名 + 密钥 + 至少一个 peer)。
func (c Config) Configured() bool {
	return c.Join.NetworkName != "" && c.Join.NetworkSecret != "" && len(c.Join.Peers) > 0
}

func dedupeTrim(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
