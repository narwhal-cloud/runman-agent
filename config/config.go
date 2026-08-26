package config

import (
	"encoding/json"
	"os"
	"strings"
	"sync"

	"golang.org/x/crypto/bcrypt"
)

const (
	DefaultDB             = "/opt/narwhal-agent/agent.db"
	DefaultWeb            = ":8792"
	DefaultMaxPortForward = 20
	DefaultWebUser        = "admin"
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

	// 每个 VM 端口转发允许的最大唯一来源 IP 数（全局默认值，0 表示不限制，可按 VM 覆盖）
	MaxForwardIPs int32 `json:"max_forward_ips"`

	// 每月自动重置流量的日期（1-28）
	TrafficResetDay int `json:"traffic_reset_day"`

	// IPv6 配置（cloud-hypervisor / incus 公用）
	IPv6Mode   string `json:"ipv6_mode"`   // "none"/"subnet"/"snat"
	IPv6Subnet string `json:"ipv6_subnet"` // 可用子网，如 "2001:db8:1234::/64"
	IPv6Addr   string `json:"ipv6_addr"`   // 单个公网 IPv6（snat 模式）
	IPv6Iface  string `json:"ipv6_iface"`  // IPv6 所在网卡

	// 供 WireGuard 隧道分配的 IPv6 池（CIDR，如 "2001:db8:1234::/64"）。
	// 探测到本机/上游 IPv6 后由安装脚本按 /64 或更小前缀切分写入，
	// wgbind 在创建隧道时可从中取一个地址分配给隧道接口。
	IPv6WGSubnet string `json:"ipv6_wg_subnet"`

	// IPv6 配置备份目录（backup_ipv6_config / restore_ipv6_config 使用）
	IPv6BackupDir string `json:"ipv6_backup_dir"`

	// NDP 应答器配置（cloud-hypervisor 公网 IPv6 场景）
	NdpIface   string `json:"ndp_iface"`   // 上行网卡
	NdpSubnets string `json:"ndp_subnets"` // 响应的 IPv6 CIDR，逗号分隔
	NdpNetwork string `json:"ndp_network"` // Podman 网络名（Podman NDP 场景）

	// rfw 防火墙 API 本地监听地址（agent 反代用）
	RfwAddr string `json:"rfw_addr"`

	// ── Incus 定制能力 ──────────────────────────────────────────────────────────
	// 自定义欢迎页 / SSH 登录横幅
	IncusBannerPreset string `json:"incus_banner_preset"` // "none" / "default" / "project" / "custom"
	IncusBannerText   string `json:"incus_banner_text"`   // preset="custom" 时的完整文本内容

	// 每个容器分配的 IPv6 数量（非 /64 网段精细化分配，默认 1）
	IncusIPv6Alloc int32 `json:"incus_ipv6_alloc"`

	// 自定义 alpine 基础镜像别名（结合 podcctv/alpine-base 等定制镜像）。
	// 非空时 OsImage=="alpine" 将使用该别名而非内置的 alpine/3.23/cloud。
	IncusAlpineBase string `json:"incus_alpine_base"`

	// 本地/内网 incus 镜像服务基址（覆盖 GitHub releases）。
	// 可填 http(s) URL（如 http://192.168.10.9:8080）或本地目录路径。
	IncusImageMirror string `json:"incus_image_mirror"`
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

	m := &Manager{path: path, cfg: cfg}

	// 如果密码不是加密的（即首次安装由安装脚本写入的明文），自动进行 bcrypt 加密并写回
	if cfg.WebPassHash != "" && !strings.HasPrefix(cfg.WebPassHash, "$2") {
		err = m.Update(func(c *Config) {
			hash, _ := bcrypt.GenerateFromPassword([]byte(c.WebPassHash), bcrypt.DefaultCost)
			c.WebPassHash = string(hash)
		})
		if err != nil {
			return nil, err
		}
	}

	return m, nil
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
	if cfg.WebUser == "" {
		cfg.WebUser = DefaultWebUser
	}
	if cfg.MaxPortForward <= 0 {
		cfg.MaxPortForward = DefaultMaxPortForward
	}
	if cfg.TrafficResetDay <= 0 || cfg.TrafficResetDay > 28 {
		cfg.TrafficResetDay = 1
	}
	if cfg.IPv6Mode == "" {
		cfg.IPv6Mode = "none"
	}
	if cfg.RfwAddr == "" {
		cfg.RfwAddr = "127.0.0.1:7734"
	}
	if cfg.IncusIPv6Alloc <= 0 {
		cfg.IncusIPv6Alloc = 1
	}
	if cfg.IncusBannerPreset == "" {
		cfg.IncusBannerPreset = "none"
	}
	if cfg.IPv6BackupDir == "" {
		cfg.IPv6BackupDir = "/var/lib/narwhal-agent/backups"
	}
}
