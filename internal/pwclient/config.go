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
// 认证二选一:NetworkSecret(长期凭据,正式成员)或 Credential(临时凭据,Secure Mode
// 签发;过期/撤销后需重新导入)。两者都有时凭据优先。
//
// 注:客户端始终以 Secure Mode 组网(--secure-mode),不接受配置关闭。服务端 Secure
// Mode 开启后混合模式会丢数据包(握手成功但访问不了网络);平台导出文件里的
// secureMode 字段仅记录服务端状态,客户端不消费(旧配置里的 false 会被忽略)。
type JoinConfig struct {
	NetworkName   string   `yaml:"networkName"`
	NetworkSecret string   `yaml:"networkSecret"` // 明文落盘(0600);绝不入日志/状态回显
	Credential    string   `yaml:"credential"`    // 临时凭据入网(隐含 Secure Mode)
	Peers         []string `yaml:"peers"`         // 至少一个公网可达的对端地址(如 tcp://IP:11010)
	VirtualIP     string   `yaml:"virtualIp"`     // 空 = DHCP 自动分配
	Hostname      string   `yaml:"hostname"`      // 空 = 计算机名
}

// EasyTierConfig 是 easytier 二进制定位与本地 RPC 状态口。
type EasyTierConfig struct {
	BinDir  string `yaml:"binDir"`  // 空 = <dataDir>/bin
	RPCAddr string `yaml:"rpcAddr"` // 空 = 127.0.0.1:15889(避开平台节点默认的 15888)
}

// Socks5Config 是本地 SOCKS5 服务(easytier --socks5):应用把代理指向
// 127.0.0.1:<port> 即可经集群虚拟网络访问(含经 NAT 的远端网段),不动系统路由,
// 与「网段路由」互补。注意 EasyTier 侧无认证 —— 仅本机使用(客户端防火墙阻断非本机访问)。
type Socks5Config struct {
	Enabled bool `yaml:"enabled"`
	Port    int  `yaml:"port"` // 空/0 且 enabled = 1080;disabled 时端口保留不清空
	// ExitNodes 是出口节点虚拟 IP 列表(序即优先级;空 = 不使用出口节点)。设置后
	// SOCKS5 拨向非虚拟网目标的流量经所选节点 NAT 出网;目标节点须显式允许充当出口。
	ExitNodes []string `yaml:"exitNodes"`
}

// socks5DefaultPort 是启用 SOCKS5 而未指定端口时的默认端口。
const socks5DefaultPort = 1080

// AliyunGroupConfig 是一个待自动更新的阿里云安全组。Description 是匹配锚点:同步时
// 在该安全组的既有规则里按描述精确匹配(须组内唯一),协议/端口沿用旧规则,只把源 IP
// 换成本机当前公网 IP。
type AliyunGroupConfig struct {
	RegionID        string `yaml:"regionId"`
	SecurityGroupID string `yaml:"securityGroupId"`
	Description     string `yaml:"description"`
}

// AliyunConfig 是「连接前自动更新阿里云安全组」的配置(可选;不配 = 不启用)。更新方式:
// 连接/重连前把本机公网 IP 授权进各组匹配描述的规则并回收旧 IP(先加新再删旧),
// 连接保持期间定期复查自愈。密钥明文落盘(config.yaml 整体 0600);绝不入日志/状态回显。
type AliyunConfig struct {
	AccessKeyID     string              `yaml:"accessKeyId"`
	AccessKeySecret string              `yaml:"accessKeySecret"`
	Groups          []AliyunGroupConfig `yaml:"groups"`
}

// ProxyConfig 是一条独立的多出口代理:EasyTier 单进程仅一个 --socks5 端口且出口是
// 节点级路由,故每条配置由运行时拉起一个独立轻量组网实例(独立实例目录 + DHCP 虚拟 IP),
// 本地端口与出口节点一一绑定。ExitNodes 空 = 该端口仅访问虚拟网/通告网段,不出网。
type ProxyConfig struct {
	Port      int      `yaml:"port"`
	ExitNodes []string `yaml:"exitNodes"`
}

// Config 是客户端全部配置(config.yaml;服务与 GUI 共享读写)。
type Config struct {
	// Enabled 是连接开关(托盘「断开/连接」);false = 停止组网、保留配置。
	Enabled bool       `yaml:"enabled"`
	Join    JoinConfig `yaml:"join"`
	// Routes 手工限定本机路由的网段(cidr)。空 = 不路由任何通告网段(fail-closed,仅虚拟网互通);
	// 非空 = 经 --manual-routes 仅路由列表内网段。
	Routes []string     `yaml:"routes"`
	Socks5 Socks5Config `yaml:"socks5"`
	// Proxies 是附加的多出口代理列表(每条 = 独立本地 SOCKS5 端口 + 各自出口节点)。
	Proxies  []ProxyConfig  `yaml:"proxies"`
	EasyTier EasyTierConfig `yaml:"easytier"`
	// Aliyun 是连接前自动更新阿里云安全组的配置(可选)。不参与组网指纹:改动只重跑
	// 安全组同步,不重启 EasyTier。
	Aliyun AliyunConfig `yaml:"aliyun"`
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
	cfg.Join.Credential = strings.TrimSpace(cfg.Join.Credential)
	cfg.Join.VirtualIP = strings.TrimSpace(cfg.Join.VirtualIP)
	cfg.Join.Hostname = strings.TrimSpace(cfg.Join.Hostname)
	cfg.Join.Peers = dedupeTrim(cfg.Join.Peers)
	if cfg.Socks5.Enabled && cfg.Socks5.Port == 0 {
		cfg.Socks5.Port = socks5DefaultPort
	}
	exits, err := normalizeExitNodes(cfg.Socks5.ExitNodes)
	if err != nil {
		return cfg, err
	}
	cfg.Socks5.ExitNodes = exits
	if cfg.Socks5.Port < 0 || cfg.Socks5.Port > 65535 {
		return cfg, fmt.Errorf("%w: SOCKS5 端口 %d 超出范围(1-65535)", ErrInvalidConfig, cfg.Socks5.Port)
	}
	if err := normalizeProxies(&cfg); err != nil {
		return cfg, err
	}
	if err := normalizeAliyun(&cfg); err != nil {
		return cfg, err
	}
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

// normalizeExitNodes 归一化出口节点列表:去空白、去重(保序)、逐项须为合法 IP
// (填对端虚拟 IP,非 CIDR)。
func normalizeExitNodes(ips []string) ([]string, error) {
	seen := make(map[string]struct{}, len(ips))
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		ip = strings.TrimSpace(ip)
		if ip == "" {
			continue
		}
		if net.ParseIP(ip) == nil {
			return nil, fmt.Errorf("%w: 出口节点 %q 不是合法 IP(填对端虚拟 IP)", ErrInvalidConfig, ip)
		}
		if _, dup := seen[ip]; !dup {
			seen[ip] = struct{}{}
			out = append(out, ip)
		}
	}
	return out, nil
}

// normalizeProxies 归一化多出口代理:端口 1-65535、彼此不重复、不与主 SOCKS5 端口冲突
// (主代理启用时;禁用时不拦,启用时再暴露)。出口列表逐条归一。保序(列表序即实例序)。
func normalizeProxies(cfg *Config) error {
	seenPort := make(map[int]struct{}, len(cfg.Proxies))
	out := make([]ProxyConfig, 0, len(cfg.Proxies))
	for _, p := range cfg.Proxies {
		if p.Port < 1 || p.Port > 65535 {
			return fmt.Errorf("%w: 多出口代理端口 %d 超出范围(1-65535)", ErrInvalidConfig, p.Port)
		}
		if _, dup := seenPort[p.Port]; dup {
			return fmt.Errorf("%w: 多出口代理端口 %d 重复", ErrInvalidConfig, p.Port)
		}
		if cfg.Socks5.Enabled && p.Port == socks5Port(*cfg) {
			return fmt.Errorf("%w: 多出口代理端口 %d 与主 SOCKS5 端口冲突", ErrInvalidConfig, p.Port)
		}
		exits, err := normalizeExitNodes(p.ExitNodes)
		if err != nil {
			return fmt.Errorf("%w: 代理 %d 的%v", ErrInvalidConfig, p.Port, strings.TrimPrefix(err.Error(), ErrInvalidConfig.Error()+": "))
		}
		seenPort[p.Port] = struct{}{}
		out = append(out, ProxyConfig{Port: p.Port, ExitNodes: exits})
	}
	cfg.Proxies = out
	return nil
}

// normalizeAliyun 归一化阿里云安全组段:去空白、丢全空行、逐组三字段必填(缺即报错,
// 配置来自 GUI/手输须即时反馈)、按 (regionId, securityGroupId) 去重(同组多描述无意义
// —— 描述是组内锚点,同步时一次 Describe 拿全)。配了组但 AK 不成对 → 报错;组为空时
// AK 残留不报错(先填一半的场景,不拦)。密钥仅去空白,不校验内容。
func normalizeAliyun(cfg *Config) error {
	a := &cfg.Aliyun
	a.AccessKeyID = strings.TrimSpace(a.AccessKeyID)
	a.AccessKeySecret = strings.TrimSpace(a.AccessKeySecret)
	out := make([]AliyunGroupConfig, 0, len(a.Groups))
	seen := make(map[string]struct{}, len(a.Groups))
	for _, g := range a.Groups {
		g.RegionID = strings.TrimSpace(g.RegionID)
		g.SecurityGroupID = strings.TrimSpace(g.SecurityGroupID)
		g.Description = strings.TrimSpace(g.Description)
		if g.RegionID == "" && g.SecurityGroupID == "" && g.Description == "" {
			continue
		}
		if g.RegionID == "" || g.SecurityGroupID == "" || g.Description == "" {
			return fmt.Errorf("%w: 安全组行(regionId=%q, securityGroupId=%q, description=%q)须三项都填写",
				ErrInvalidConfig, g.RegionID, g.SecurityGroupID, g.Description)
		}
		key := g.RegionID + "|" + g.SecurityGroupID
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, g)
	}
	a.Groups = out
	if len(out) > 0 && (a.AccessKeyID == "" || a.AccessKeySecret == "") {
		return fmt.Errorf("%w: 已配置安全组但缺少阿里云 AccessKeyId/AccessKeySecret(须成对)", ErrInvalidConfig)
	}
	return nil
}

// AliyunReady 报告安全组同步是否具备完整配置(AK 成对 + 至少一组)。
func (c Config) AliyunReady() bool {
	return c.Aliyun.AccessKeyID != "" && c.Aliyun.AccessKeySecret != "" && len(c.Aliyun.Groups) > 0
}

// socks5Port 取生效的 SOCKS5 端口(未启用 = 0)。
func socks5Port(cfg Config) int {
	if cfg.Socks5.Enabled {
		return cfg.Socks5.Port
	}
	return 0
}

// ImportExport 解析平台导出的 pipewright-client.yaml,产出可合并的加入参数与预填路由。
// 仅返回 join 与 routes 两个可合并段;本机既有配置的 enabled/easytier 段由调用方保留。
// 认证段兼容两种形态:networkSecret(长期)或 credential(临时凭据,手工替换/平台凭据
// 导出),两者至少其一。
func ImportExport(raw []byte) (join JoinConfig, routes []string, meta exportMeta, err error) {
	var f exportFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return join, nil, meta, fmt.Errorf("%w: 不是合法的导出文件: %v", ErrInvalidConfig, err)
	}
	if strings.TrimSpace(f.Join.NetworkName) == "" ||
		(strings.TrimSpace(f.Join.NetworkSecret) == "" && strings.TrimSpace(f.Join.Credential) == "") {
		return join, nil, meta, fmt.Errorf("%w: 导出文件缺少网络名或入网凭据(组网密钥/临时凭据)", ErrInvalidConfig)
	}
	cfg, err := Normalize(Config{Join: f.Join, Routes: f.Routes})
	if err != nil {
		return join, nil, meta, err
	}
	return cfg.Join, cfg.Routes, f.Export, nil
}

// Configured 报告加入参数是否已配置完整(网络名 + 任一入网凭据 + 至少一个 peer)。
func (c Config) Configured() bool {
	hasAuth := c.Join.NetworkSecret != "" || c.Join.Credential != ""
	return c.Join.NetworkName != "" && hasAuth && len(c.Join.Peers) > 0
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
