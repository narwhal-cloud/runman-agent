package config

import (
	"encoding/json"
	"os"
	"sync"
)

const (
	DefaultDB             = "/opt/narwhal-agent/agent.db"
	DefaultWeb            = ":8792"
	DefaultMaxPortForward = 20
	DefaultDiskMbps       = 100
)

// Config 是 agent 的全局配置，从 JSON 配置文件读取，通过 Web API 可修改。
type Config struct {
	DB  string `json:"db"`
	Web string `json:"web"`

	// 虚拟化类型（podman/cloudhv），首次安装时由配置文件写入，不通过 Web API 修改
	VirtType string `json:"virt_type"`

	// 平台认证
	Token string `json:"token"`

	// 监控配置
	MonitorNIC  string `json:"monitor_nic"`
	MonitorDisk string `json:"monitor_disk"`

	// Web 面板认证
	WebUser     string `json:"web_user"`
	WebPassHash string `json:"web_pass_hash"` // bcrypt hash

	// 公网入口地址（空则由服务端自取）
	Host string `json:"host"`

	// 每个 VM 最大端口转发数
	MaxPortForward int32 `json:"max_port_forward"`

	// 虚拟机磁盘写入速率限制（Mbps）
	DiskMbps int `json:"disk_mbps"`

	// IPv6 配置（cloud-hypervisor 专用）
	IPv6Mode   string `json:"ipv6_mode"`   // "none"/"subnet"/"snat"
	IPv6Subnet string `json:"ipv6_subnet"` // 可用子网，如 "2001:db8:1234::/64"
	IPv6Addr   string `json:"ipv6_addr"`   // 单个公网 IPv6（snat 模式）
	IPv6Iface  string `json:"ipv6_iface"`  // IPv6 所在网卡

	// NDP 应答器配置（cloud-hypervisor 公网 IPv6 场景）
	NdpIface   string `json:"ndp_iface"`   // 上行网卡
	NdpSubnets string `json:"ndp_subnets"` // 响应的 IPv6 CIDR，逗号分隔
	NdpNetwork string `json:"ndp_network"` // Podman 网络名（Podman NDP 场景）
}

// Manager 持有配置文件路径和内存副本，提供并发安全的读写。
type Manager struct {
	path string
	mu   sync.RWMutex
	cfg  Config
}

// Load 从 JSON 配置文件加载配置到内存。
func Load(path string) (*Manager, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	applyDefaults(&cfg)
	return &Manager{path: path, cfg: cfg}, nil
}

// Get 返回当前配置的快照副本（并发安全）。
func (m *Manager) Get() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

// Update 原子更新配置内存值并持久化到文件。
// fn 接收指针并在锁内修改，修改完成后写文件。
func (m *Manager) Update(fn func(*Config)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn(&m.cfg)
	return m.save()
}

// save 将当前内存配置写回文件（调用方持有写锁）。
func (m *Manager) save() error {
	data, err := json.MarshalIndent(m.cfg, "", "    ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.path, data, 0600)
}

// applyDefaults 在字段为空时填充默认值。
func applyDefaults(cfg *Config) {
	if cfg.DB == "" {
		cfg.DB = DefaultDB
	}
	if cfg.Web == "" {
		cfg.Web = DefaultWeb
	}
	if cfg.MaxPortForward <= 0 {
		cfg.MaxPortForward = DefaultMaxPortForward
	}
	if cfg.DiskMbps <= 0 {
		cfg.DiskMbps = DefaultDiskMbps
	}
	if cfg.IPv6Mode == "" {
		cfg.IPv6Mode = "none"
	}
}
