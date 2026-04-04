package cloudhv

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"runman-agent/db"
	"runman-agent/manager"
	"runman-agent/proto/agent"
)

const (
	chBinary       = "/opt/narwhal-agent/cloud-hypervisor" // cloud-hypervisor 二进制路径（固定）
	imgDir         = "/opt/vm-images"
	runDir         = "/run/cloud-hypervisor"
	bridge         = "narwhal-net"
	gwIPv4         = "10.91.0.1"
	netCIDRv4      = "10.91.0.0/20"
	gwIPv6Local    = "fd91:cafe:cafe:10::1"
	netCIDRv6Local = "fd91:cafe:cafe:10::/64"
	tapBase        = "vm" // TAP name: vm2, vm3, ...
)

// Manager 实现 VMManager 接口，管理多个 cloud-hypervisor 进程（每个 VM 一个进程）。
type Manager struct {
	binary      string // cloud-hypervisor 二进制路径
	instDir     string // VM 实例数据目录 (imgDir/instances)
	db          *db.DB
	ipv6Mode    string // "none"/"subnet"/"snat"
	ipv6Subnet  string // 可用子网，如 "2001:db8:1234::/64"（mode=subnet）
	ipv6Addr    string // 单个公网 IPv6（mode=snat）
	ipv6Iface   string // IPv6 所在网卡
	ipv6Counter int    // 子网模式下的地址计数器（用于分配 ::2, ::3 等）
}

// --- REST API 请求/响应结构体 ---

type vmCreateReq struct {
	Cpus    cpusConfig     `json:"cpus"`
	Memory  memoryConfig   `json:"memory"`
	Payload payloadConfig  `json:"payload"`
	Disks   []diskConfig   `json:"disks"`
	Net     []netCfg       `json:"net"`
	Rng     *rngConfig     `json:"rng,omitempty"`
	Balloon *balloonConfig `json:"balloon,omitempty"`
	Serial  *consoleConfig `json:"serial,omitempty"`
	Console *consoleConfig `json:"console,omitempty"`
}

type cpusConfig struct {
	BootVcpus int `json:"boot_vcpus"`
	MaxVcpus  int `json:"max_vcpus"`
}

type memoryConfig struct {
	Size          int64  `json:"size"`
	Mergeable     bool   `json:"mergeable"`
	Thp           bool   `json:"thp"`
	HotplugMethod string `json:"hotplug_method,omitempty"`
	HotplugSize   *int64 `json:"hotplug_size,omitempty"`
}

type balloonConfig struct {
	Size              int64 `json:"size"`
	DeflateOnOom      bool  `json:"deflate_on_oom"`
	FreePageReporting bool  `json:"free_page_reporting"`
}

type payloadConfig struct {
	Kernel    string `json:"kernel"`
	Initramfs string `json:"initramfs,omitempty"`
	Cmdline   string `json:"cmdline"`
}

type diskConfig struct {
	Path              string             `json:"path"`
	Readonly          bool               `json:"readonly,omitempty"`
	Direct            bool               `json:"direct,omitempty"`
	NumQueues         int                `json:"num_queues,omitempty"`
	RateLimiterConfig *rateLimiterConfig `json:"rate_limiter_config,omitempty"`
}

type netCfg struct {
	Tap               string             `json:"tap"`
	Mac               string             `json:"mac"`
	ID                string             `json:"id,omitempty"`
	RateLimiterConfig *rateLimiterConfig `json:"rate_limiter_config,omitempty"`
}

type rateLimiterConfig struct {
	Bandwidth *tokenBucket `json:"bandwidth,omitempty"`
	Ops       *tokenBucket `json:"ops,omitempty"`
}

type tokenBucket struct {
	Size         int64 `json:"size"`
	OneTimeBurst int64 `json:"one_time_burst,omitempty"`
	RefillTime   int64 `json:"refill_time"` // ms
}

type vmRemoveDevice struct {
	ID string `json:"id"`
}

type rngConfig struct {
	Src string `json:"src"`
}

type consoleConfig struct {
	Mode   string `json:"mode"`
	File   string `json:"file,omitempty"`
	Socket string `json:"socket,omitempty"`
}

type vmInfoResp struct {
	State string `json:"state"` // Created / Running / Shutdown / Paused
}

type vmResizeReq struct {
	DesiredVcpus *int   `json:"desired_vcpus,omitempty"`
	DesiredRam   *int64 `json:"desired_ram,omitempty"`
}

// 实例网络配置（从 DB CloudHVVMConfig 构造）
type netConfig struct {
	Idx  int
	Tap  string
	MAC  string
	IPv4 string
	IPv6 string // 可能为空
}

// 实例配置（存储在 instDir/<vmID>/config.json）
type instanceConfig struct {
	Distro        string `json:"distro"`
	CPU           int    `json:"cpu"`
	MemoryMB      int64  `json:"memory_mb"`
	DiskGB        int64  `json:"disk_gb"`
	BandwidthMbps int    `json:"bandwidth_mbps"` // 0 表示不限速
}

// New 创建 Manager。
func New(database *db.DB) (*Manager, error) {
	if _, err := os.Stat(chBinary); err != nil {
		return nil, fmt.Errorf("cloud-hypervisor binary not found at %q: %w", chBinary, err)
	}
	instDir := filepath.Join(imgDir, "instances")
	for _, d := range []string{instDir, runDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	// 从 DB 读取 IPv6 配置
	conf, _ := database.GetConfig()
	m := &Manager{
		binary:      chBinary,
		instDir:     instDir,
		db:          database,
		ipv6Mode:    "none",
		ipv6Counter: 2, // 从 ::2 开始分配
	}
	if conf != nil {
		m.ipv6Mode = conf.IPv6Mode
		m.ipv6Subnet = conf.IPv6Subnet
		m.ipv6Addr = conf.IPv6Addr
		m.ipv6Iface = conf.IPv6Iface
	}

	if err := m.ensureBridge(); err != nil {
		return nil, fmt.Errorf("setup bridge: %w", err)
	}
	return m, nil
}

func findBinary() string {
	for _, c := range []string{
		"./scripts/cloud-hypervisor-static",
		"./cloud-hypervisor-static",
		"/opt/narwhal-agent/cloud-hypervisor",
		"/usr/local/bin/cloud-hypervisor",
		"/usr/bin/cloud-hypervisor",
	} {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return "./cloud-hypervisor-static"
}

// ensureBridge 每次 agent 启动时幂等地恢复网络环境
func (m *Manager) ensureBridge() error {
	// 1. 创建 bridge narwhal-net（若已存在则跳过）
	if err := runCmd("ip", "link", "show", bridge); err != nil {
		if err := runCmd("ip", "link", "add", bridge, "type", "bridge"); err != nil {
			return fmt.Errorf("create bridge: %w", err)
		}
	}
	// 关闭多播嗅探，防止网桥丢弃 IPv6 NDP (Neighbor Discovery) 报文
	_ = os.WriteFile(fmt.Sprintf("/sys/class/net/%s/bridge/multicast_snooping", bridge), []byte("0"), 0644)

	// 2. 配置 IPv4 地址（幂等：直接尝试添加，忽略已存在错误）
	_ = runCmd("ip", "addr", "add", gwIPv4+"/20", "dev", bridge)

	// 3. 启动 bridge
	_ = runCmd("ip", "link", "set", bridge, "up")

	// 4. 启用 IPv4 转发
	_ = os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644)

	// 5. NAT masquerade（IPv4）及 FORWARD 放行
	if outIf := defaultRouteIface(); outIf != "" {
		cidr := netCIDRv4
		if runCmd("iptables", "-t", "nat", "-C", "POSTROUTING", "-s", cidr, "-o", outIf, "-j", "MASQUERADE") != nil {
			_ = runCmd("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", cidr, "-o", outIf, "-j", "MASQUERADE")
		}
		// 确保 FORWARD 链放行 bridge 流量 (IPv4)
		_ = runCmd("iptables", "-I", "FORWARD", "-i", bridge, "-j", "ACCEPT")
		_ = runCmd("iptables", "-I", "FORWARD", "-o", bridge, "-j", "ACCEPT")
	}

	// 6. IPv6 支持配置及 FORWARD 放行
	_ = os.WriteFile("/proc/sys/net/ipv6/conf/all/forwarding", []byte("1"), 0644)
	_ = os.WriteFile("/proc/sys/net/ipv6/conf/all/proxy_ndp", []byte("1"), 0644)
	_ = os.WriteFile("/proc/sys/net/ipv6/conf/default/forwarding", []byte("1"), 0644)
	_ = os.WriteFile("/proc/sys/net/ipv6/conf/default/proxy_ndp", []byte("1"), 0644)

	outIf := defaultRouteIface()
	if outIf != "" {
		_ = os.WriteFile(fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/forwarding", outIf), []byte("1"), 0644)
		_ = os.WriteFile(fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/proxy_ndp", outIf), []byte("1"), 0644)
	}

	// 确保 FORWARD 链放行 bridge 流量 (IPv6)
	_ = runCmd("ip6tables", "-I", "FORWARD", "-i", bridge, "-j", "ACCEPT")
	_ = runCmd("ip6tables", "-I", "FORWARD", "-o", bridge, "-j", "ACCEPT")

	// 7. IPv6 SNAT（如果是 SNAT 模式）
	if m.ipv6Mode == "snat" && m.ipv6Iface != "" {
		// 配置 IPv6 MASQUERADE
		// ULA 网段出站（如果源地址是 ULA，SNAT 到宿主机 IPv6）
		cidr6 := netCIDRv6Local
		if runCmd("ip6tables", "-t", "nat", "-C", "POSTROUTING", "-s", cidr6, "-o", m.ipv6Iface, "-j", "MASQUERADE") != nil {
			_ = runCmd("ip6tables", "-t", "nat", "-A", "POSTROUTING", "-s", cidr6, "-o", m.ipv6Iface, "-j", "MASQUERADE")
		}
	}

	// 8. subnet模式：配置bridge的IPv6地址用于NDP（网关地址）和路由规则
	if m.ipv6Mode == "subnet" && m.ipv6Subnet != "" {
		// 从子网中计算网关地址：subnet::1
		// 例如：2a13:7e80:0:387::/64 → 2a13:7e80:0:387::1/112
		parts := strings.SplitN(m.ipv6Subnet, "::/", 2)
		if len(parts) == 2 {
			prefix := parts[0]
			// 确保没有多余的":"
			prefix = strings.TrimSuffix(prefix, ":")
			gwCIDR := prefix + "::1/112"
			routeCIDR := prefix + "::/112"

			// 直接尝试添加（忽略已存在错误）
			_ = runCmd("ip", "-6", "addr", "add", gwCIDR, "dev", bridge)

			// 添加IPv6路由：确保/112段的包被送到bridge，优先级高于uplink的/64
			// 这是关键，避免与eth0的/64路由冲突
			_ = runCmd("ip", "-6", "route", "replace", routeCIDR, "dev", bridge, "metric", "256")

			log.Printf("info: ensured IPv6 gateway %s on %s, route %s", gwCIDR, bridge, routeCIDR)
		}
	}

	return nil
}

// ─── 网络辅助 ─────────────────────────────────────────────────────────────────

func (m *Manager) allocNetwork(vmID string) (*netConfig, error) {
	// 尝试从 DB 复用已有配置
	if conf, err := m.db.GetCloudHVConfig(vmID); err == nil {
		return m.netConfigFromDB(conf), nil
	}

	// 分配新 idx
	idx, err := m.db.NextCloudHVIdx()
	if err != nil {
		return nil, fmt.Errorf("no free IP indices available: %w", err)
	}

	// 计算 IPv4：10.91.{idx>>8}.{idx&0xff}
	host := idx & 0xff
	subnet := (idx >> 8) & 0xf
	ipv4 := fmt.Sprintf("10.91.%d.%d", subnet, host)

	// 计算 IPv6
	ipv6 := m.computeIPv6(idx)

	// 计算 MAC：52:54:00:<idx_high>:<idx_low>:01
	mac := fmt.Sprintf("52:54:00:%02x:%02x:01", (idx>>8)&0xff, idx&0xff)

	// 保存到 DB
	conf := &db.CloudHVVMConfig{
		VMID: vmID,
		Idx:  idx,
		MAC:  mac,
		IPv4: ipv4,
		IPv6: ipv6,
	}
	if err := m.db.SaveCloudHVConfig(conf); err != nil {
		return nil, fmt.Errorf("save network config: %w", err)
	}

	return m.netConfigFromDB(conf), nil
}

func (m *Manager) netConfigFromDB(conf *db.CloudHVVMConfig) *netConfig {
	return &netConfig{
		Idx:  conf.Idx,
		Tap:  fmt.Sprintf("%s%d", tapBase, conf.Idx),
		MAC:  conf.MAC,
		IPv4: conf.IPv4,
		IPv6: conf.IPv6,
	}
}

// computeIPv6 计算 VM 的 IPv6 地址，根据配置返回三种模式之一
// idx 用于从子网中分配不同的地址
func (m *Manager) computeIPv6(idx int) string {
	switch m.ipv6Mode {
	case "subnet":
		// 从子网中分配独立 IPv6
		// 例如：2001:db8:1234::/64 → 2001:db8:1234::2, ::3, ...
		if m.ipv6Subnet != "" {
			// 解析子网，计算第 idx 个地址
			return m.allocIPv6FromSubnet(idx)
		}
	case "snat":
		// SNAT 模式：所有 VM 共享宿主机的 IPv6 出站，不分配独立地址
		return ""
	}
	// 默认：使用 ULA 段（即使 mode=none 或 mode=""）
	return fmt.Sprintf("fd91:cafe:cafe:10::%d", idx)
}

// allocIPv6FromSubnet 从子网中分配第 idx 个 IPv6 地址
// 例如：2001:db8:1234::/64 + idx=2 → 2001:db8:1234::2
func (m *Manager) allocIPv6FromSubnet(idx int) string {
	// 简化实现：假设子网格式为 "prefix::/bits"
	// 从子网的网络地址 +1（跳过网络地址本身）开始分配
	parts := strings.SplitN(m.ipv6Subnet, "::/", 2)
	if len(parts) == 2 {
		prefix := parts[0]
		// 移除最后的 ":"，然后拼接 idx
		prefix = strings.TrimSuffix(prefix, ":")
		return fmt.Sprintf("%s::%d", prefix, idx)
	}
	// 备选：如果格式不符合，返回 ULA
	return fmt.Sprintf("fd91:cafe:cafe:10::%d", idx)
}

func (m *Manager) setupNetwork(net *netConfig) error {
	// 创建 TAP 设备（若已存在则跳过）
	tapExists := runCmd("ip", "link", "show", net.Tap) == nil
	if !tapExists {
		if err := runCmd("ip", "tuntap", "add", net.Tap, "mode", "tap"); err != nil {
			return fmt.Errorf("create tap %s: %w", net.Tap, err)
		}
	}

	// 加入 bridge 并启动
	_ = runCmd("ip", "link", "set", net.Tap, "master", bridge)
	_ = runCmd("ip", "link", "set", net.Tap, "up")

	// 开启 TAP 设备的 proxy_ndp，帮助 VM 和外部跨网桥顺畅沟通
	_ = os.WriteFile(fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/proxy_ndp", net.Tap), []byte("1"), 0644)
	_ = os.WriteFile(fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/forwarding", net.Tap), []byte("1"), 0644)

	log.Printf("[setupNetwork] TAP device %s setup complete (MAC: %s)", net.Tap, net.MAC)

	// IPv6 模式特定配置
	if net.IPv6 != "" {
		switch m.ipv6Mode {
		case "subnet":
			// subnet模式：
			// 1. 在uplink接口上配置neighbor proxy（让外网知道这个地址可达）
			// 2. 添加IPv6路由：VM地址通过TAP可达
			// (不要在bridge上添加VM的地址，会导致DAD冲突)

			outIf := defaultRouteIface()
			log.Printf("[setupNetwork] IPv6 subnet mode setup for %s, outIf=%s", net.IPv6, outIf)

			// 配置neighbor proxy
			if outIf != "" {
				// 尝试添加
				err1 := runCmd("ip", "-6", "neigh", "add", "proxy", net.IPv6, "dev", outIf)
				if err1 != nil {
					// 可能已存在，尝试替换
					err2 := runCmd("ip", "-6", "neigh", "replace", "proxy", net.IPv6, "dev", outIf)
					log.Printf("[setupNetwork] neighbor proxy add failed, replace result: %v", err2)
				}
				// 验证是否生效
				out, _ := cmdOutput("ip", "-6", "neigh", "show", "proxy", "dev", outIf)
				if strings.Contains(out, net.IPv6) {
					log.Printf("[setupNetwork] neighbor proxy confirmed on %s", outIf)
				} else {
					log.Printf("[setupNetwork] WARNING: neighbor proxy not confirmed on %s", outIf)
				}
			}

			// 强制添加一个 /128 的路由指向网桥，确保 Linux 内核的 proxy_ndp 能够正确匹配到目标接口（必须和 outIf 不同）
			// Note: Instead of /128 per VM, we rely on the /112 route added on the bridge.
			// The /112 route on the bridge (e.g. 2a13:7e80:0:387::/112 dev narwhal-net) is enough
			// to satisfy proxy_ndp's requirement that the destination is routed to a different interface than eth0.

			log.Printf("[setupNetwork] IPv6 subnet mode: VM %s via TAP %s + proxy on %s", net.IPv6, net.Tap, outIf)

		case "snat":
			// SNAT 配置（首次全局配置，这里可选）
			// 实际配置在 ensureBridge 中做一次就够了
		}
	}

	return nil
}

func defaultRouteIface() string {
	for _, args := range [][]string{{"route"}, {"-6", "route"}} {
		out, err := cmdOutput("ip", args...)
		if err == nil {
			for _, line := range strings.Split(out, "\n") {
				if strings.HasPrefix(line, "default") {
					parts := strings.Fields(line)
					for i, p := range parts {
						if p == "dev" && i+1 < len(parts) {
							return parts[i+1]
						}
					}
				}
			}
		}
	}
	return ""
}

// ─── 磁盘辅助 ─────────────────────────────────────────────────────────────────

func (m *Manager) prepareDisks(vmID, distro string, diskGB int64) error {
	base := filepath.Join(imgDir, distro, "current.raw")
	if _, err := os.Stat(base); err != nil {
		return fmt.Errorf("base image for %q not found: %s", distro, base)
	}
	system := filepath.Join(m.instDir, vmID, "system.raw")
	if err := runCmd("cp", "--sparse=always", base, system); err != nil {
		return fmt.Errorf("copy base image: %w", err)
	}
	if diskGB > 0 {
		if err := runCmd("qemu-img", "resize", "-f", "raw", system, fmt.Sprintf("%dG", diskGB)); err != nil {
			return fmt.Errorf("resize disk: %w", err)
		}
	}
	return nil
}

// ─── Cloud-init ────────────────────────────────────────────────────────────────

func (m *Manager) genCloudInit(vmID, distro, password string, net *netConfig) error {
	dir := filepath.Join(m.instDir, vmID)
	ciDir := filepath.Join(dir, "cloudinit")
	_ = os.MkdirAll(ciDir, 0755)

	userData := fmt.Sprintf(`#cloud-config
ssh_pwauth: true
disable_root: false
write_files:
  - path: /etc/resolv.conf
    content: |
      nameserver 1.1.1.1
    permissions: '0644'
runcmd:
  - 'echo "root:%s" | chpasswd'
  - sed -i 's/^#\?PermitRootLogin.*/PermitRootLogin yes/' /etc/ssh/sshd_config
  - systemctl restart sshd 2>/dev/null || service sshd restart 2>/dev/null || true
`, password)

	metaData := fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", vmID, vmID)

	var networkCfg string
	if distro == "alpine" {
		// Alpine: cloud-init v1 format
		networkCfg = fmt.Sprintf(`version: 1
config:
  - type: physical
    name: eth0
    mac_address: "%s"
    subnets:
      - type: static
        address: %s/20
        gateway: %s
        dns_nameservers:
          - 1.1.1.1
`, net.MAC, net.IPv4, gwIPv4)
		// 只在有独立 IPv6 或 SNAT 模式下添加 IPv6 配置
		if net.IPv6 != "" && m.ipv6Mode != "none" {
			var prefix string
			if m.ipv6Mode == "subnet" {
				// 子网模式：分配的是 /112 地址（与 podman 保持一致）
				prefix = "112"
			} else { // snat
				// SNAT 模式：VM 在 ULA 段，前缀为 /64
				prefix = "64"
			}
			networkCfg += fmt.Sprintf(`      - type: static
        address: %s/%s
        gateway: %s
        dns_nameservers:
          - 2606:4700:4700::1111
`, net.IPv6, prefix, m.ipv6GWForNet())
		}
	} else {
		// Debian/Ubuntu: netplan v2 format
		networkCfg = fmt.Sprintf(`version: 2
ethernets:
  eth0:
    match:
      macaddress: "%s"
    addresses:
      - %s/20
`, net.MAC, net.IPv4)
		// 只在有独立 IPv6 或 SNAT 模式下添加 IPv6 配置
		if net.IPv6 != "" && m.ipv6Mode != "none" {
			var prefix string
			if m.ipv6Mode == "subnet" {
				// 子网模式：分配的是 /112 地址（与 podman 保持一致）
				prefix = "112"
			} else { // snat
				// SNAT 模式：ULA 段，/64
				prefix = "64"
			}
			networkCfg += fmt.Sprintf(`      - %s/%s
`, net.IPv6, prefix)
		}
		networkCfg += fmt.Sprintf(`    gateway4: %s
`, gwIPv4)
		// 只有在有 IPv6 时才添加 IPv6 网关和 DNS6
		if net.IPv6 != "" && m.ipv6Mode != "none" {
			gw6 := m.ipv6GWForNet()
			networkCfg += fmt.Sprintf(`    gateway6: %s
`, gw6)
		}
		networkCfg += fmt.Sprintf(`    nameservers:
      addresses: [1.1.1.1, 2606:4700:4700::1111]
`)
	}

	_ = os.WriteFile(filepath.Join(ciDir, "user-data"), []byte(userData), 0644)
	_ = os.WriteFile(filepath.Join(ciDir, "meta-data"), []byte(metaData), 0644)
	_ = os.WriteFile(filepath.Join(ciDir, "network-config"), []byte(networkCfg), 0644)

	imgPath := filepath.Join(dir, "cloudinit.img")
	_ = os.Remove(imgPath)

	if _, err := exec.LookPath("cloud-localds"); err == nil {
		return runCmd("cloud-localds",
			"--network-config="+filepath.Join(ciDir, "network-config"),
			imgPath,
			filepath.Join(ciDir, "user-data"),
			filepath.Join(ciDir, "meta-data"),
		)
	}
	// 备选：mkdosfs + mcopy
	if err := runCmd("mkdosfs", "-n", "CIDATA", "-C", imgPath, "8192"); err != nil {
		return fmt.Errorf("mkdosfs: %w", err)
	}
	for _, f := range []struct{ src, dst string }{
		{filepath.Join(ciDir, "user-data"), "::user-data"},
		{filepath.Join(ciDir, "meta-data"), "::meta-data"},
		{filepath.Join(ciDir, "network-config"), "::network-config"},
	} {
		_ = runCmd("mcopy", "-oi", imgPath, f.src, f.dst)
	}
	return nil
}

// ipv6GWForNet 返回 cloud-init 网络配置中使用的 IPv6 网关
func (m *Manager) ipv6GWForNet() string {
	switch m.ipv6Mode {
	case "subnet":
		// 子网模式：网关为子网的 ::1
		if m.ipv6Subnet != "" {
			// 提取子网前缀，去掉 /前缀 部分
			parts := strings.SplitN(m.ipv6Subnet, "::/", 2)
			if len(parts) == 2 {
				prefix := strings.TrimSuffix(parts[0], ":")
				return fmt.Sprintf("%s::1", prefix)
			}
		}
	case "snat":
		// SNAT 模式：使用 ULA 网关（虚拟机通过 bridge 路由）
		return gwIPv6Local
	}
	// 默认：ULA 网关
	return gwIPv6Local
}

// ─── 进程管理 ─────────────────────────────────────────────────────────────────

func (m *Manager) sockPath(vmID string) string {
	return filepath.Join(runDir, vmID+"-api.sock")
}

func (m *Manager) serialSockPath(vmID string) string {
	return filepath.Join(runDir, vmID+"-serial.sock")
}

func (m *Manager) pidFile(vmID string) string {
	return filepath.Join(runDir, vmID+".pid")
}

func (m *Manager) isRunning(vmID string) bool {
	data, err := os.ReadFile(m.pidFile(vmID))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func (m *Manager) launchProcess(vmID string) error {
	sockPath := m.sockPath(vmID)
	_ = os.Remove(sockPath)
	_ = os.Remove(m.serialSockPath(vmID))

	cmd := exec.Command(m.binary, "--api-socket", "path="+sockPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	logFile, _ := os.OpenFile(filepath.Join(runDir, vmID+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if logFile != nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start cloud-hypervisor: %w", err)
	}
	_ = os.WriteFile(m.pidFile(vmID), []byte(strconv.Itoa(cmd.Process.Pid)), 0644)

	for i := 0; i < 60; i++ {
		if _, err := os.Stat(sockPath); err == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for API socket: %s", sockPath)
}

// ─── REST API 客户端 ───────────────────────────────────────────────────────────

func unixClient(sockPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.DialTimeout("unix", sockPath, 5*time.Second)
			},
		},
		Timeout: 60 * time.Second,
	}
}

func (m *Manager) apiPut(vmID, endpoint string, body interface{}) error {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reqBody = bytes.NewReader(data)
	}

	url := "http://localhost/api/v1/" + endpoint
	req, err := http.NewRequest(http.MethodPut, url, reqBody)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := unixClient(m.sockPath(vmID)).Do(req)
	if err != nil {
		return fmt.Errorf("API PUT %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API PUT %s: HTTP %d: %s", endpoint, resp.StatusCode, string(errBody))
	}
	return nil
}

func (m *Manager) apiGet(vmID, endpoint string, out interface{}) error {
	url := "http://localhost/api/v1/" + endpoint
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := unixClient(m.sockPath(vmID)).Do(req)
	if err != nil {
		return fmt.Errorf("API GET %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API GET %s: HTTP %d: %s", endpoint, resp.StatusCode, string(errBody))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// ─── 实例配置 ─────────────────────────────────────────────────────────────────

func (m *Manager) loadInstanceConfig(vmID string) (*instanceConfig, error) {
	data, err := os.ReadFile(filepath.Join(m.instDir, vmID, "config.json"))
	if err != nil {
		return nil, err
	}
	var cfg instanceConfig
	return &cfg, json.Unmarshal(data, &cfg)
}

func (m *Manager) saveInstanceConfig(vmID string, cfg *instanceConfig) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(m.instDir, vmID, "config.json"), data, 0644)
}

// ─── buildVmConfig 根据实例配置构造 REST API 请求 ────────────────────────────

func (m *Manager) buildVmConfig(vmID string, icfg *instanceConfig, net *netConfig) (*vmCreateReq, error) {
	distro := icfg.Distro
	vmlinuz := filepath.Join(imgDir, distro, "vmlinuz")
	initrd := filepath.Join(imgDir, distro, "initrd")
	cmdlineFile := filepath.Join(m.instDir, vmID, "cmdline")

	for _, f := range []string{vmlinuz, initrd, cmdlineFile} {
		if _, err := os.Stat(f); err != nil {
			return nil, fmt.Errorf("required file not found: %s", f)
		}
	}
	cmdlineBytes, err := os.ReadFile(cmdlineFile)
	if err != nil {
		return nil, err
	}

	cpu := icfg.CPU
	if cpu <= 0 {
		cpu = 1
	}
	memBytes := icfg.MemoryMB * 1024 * 1024
	if memBytes <= 0 {
		memBytes = 512 * 1024 * 1024
	}

	diskNumQueues := cpu
	if diskNumQueues > 4 {
		diskNumQueues = 4
	}
	disks := []diskConfig{
		{
			Path:      filepath.Join(m.instDir, vmID, "system.raw"),
			NumQueues: diskNumQueues,
		},
	}
	if _, err := os.Stat(filepath.Join(m.instDir, vmID, "cloudinit.img")); err == nil {
		disks = append(disks, diskConfig{
			Path:     filepath.Join(m.instDir, vmID, "cloudinit.img"),
			Readonly: true,
		})
	}

	hotplugSize := int64(64 * 1024 * 1024 * 1024)

	return &vmCreateReq{
		Cpus: cpusConfig{
			BootVcpus: cpu,
			MaxVcpus:  64,
		},
		Memory: memoryConfig{
			Size:          memBytes,
			Mergeable:     true,
			Thp:           true,
			HotplugMethod: "VirtioMem",
			HotplugSize:   &hotplugSize,
		},
		Payload: payloadConfig{
			Kernel:    vmlinuz,
			Initramfs: initrd,
			Cmdline:   strings.TrimSpace(string(cmdlineBytes)),
		},
		Disks: disks,
		Net: []netCfg{{
			Tap:               net.Tap,
			Mac:               net.MAC,
			ID:                "net0",
			RateLimiterConfig: buildNetRateLimit(icfg.BandwidthMbps),
		}},
		Rng: &rngConfig{Src: "/dev/urandom"},
		Balloon: &balloonConfig{
			Size:              memBytes / 2,
			DeflateOnOom:      true,
			FreePageReporting: true,
		},
		Serial: &consoleConfig{
			Mode:   "Socket",
			Socket: m.serialSockPath(vmID),
		},
		Console: &consoleConfig{
			Mode: "Off",
		},
	}, nil
}

func (m *Manager) CreateVM(ctx context.Context, req *agent.CmdCreateVM) error {
	dir := filepath.Join(m.instDir, req.VmId)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	icfg := &instanceConfig{
		Distro:        req.OsImage,
		CPU:           int(req.Cpu),
		MemoryMB:      req.RamMb,
		DiskGB:        req.DiskGb,
		BandwidthMbps: int(req.BandwidthMbps),
	}

	if err := m.saveInstanceConfig(req.VmId, icfg); err != nil {
		return err
	}

	srcCmdline := filepath.Join(imgDir, req.OsImage, "cmdline")
	cmdlineData, readErr := os.ReadFile(srcCmdline)
	if readErr != nil {
		return fmt.Errorf("cmdline not found for distro %q: %w", req.OsImage, readErr)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), cmdlineData, 0644); err != nil {
		return err
	}

	netCfg, err := m.allocNetwork(req.VmId)
	if err != nil {
		return err
	}

	if err := m.prepareDisks(req.VmId, req.OsImage, req.DiskGb); err != nil {
		return err
	}

	if err := m.genCloudInit(req.VmId, req.OsImage, req.RootPassword, netCfg); err != nil {
		return err
	}

	if err := m.setupNetwork(netCfg); err != nil {
		return err
	}

	if err := m.launchProcess(req.VmId); err != nil {
		return err
	}

	vmCfg, err := m.buildVmConfig(req.VmId, icfg, netCfg)
	if err != nil {
		return err
	}
	if err := m.apiPut(req.VmId, "vm.create", vmCfg); err != nil {
		return err
	}

	if err := m.apiPut(req.VmId, "vm.boot", nil); err != nil {
		return err
	}

	return nil
}

func (m *Manager) StartVM(_ context.Context, vmID string) error {
	if !m.isRunning(vmID) {
		icfg, err := m.loadInstanceConfig(vmID)
		if err != nil {
			return fmt.Errorf("instance %q not found: %w", vmID, err)
		}
		netCfg, err := m.allocNetwork(vmID)
		if err != nil {
			return err
		}
		if err := m.setupNetwork(netCfg); err != nil {
			return err
		}
		if err := m.launchProcess(vmID); err != nil {
			return err
		}
		vmCfg, err := m.buildVmConfig(vmID, icfg, netCfg)
		if err != nil {
			return err
		}
		if err := m.apiPut(vmID, "vm.create", vmCfg); err != nil {
			return err
		}
		return m.apiPut(vmID, "vm.boot", nil)
	}

	var info vmInfoResp
	if err := m.apiGet(vmID, "vm.info", &info); err != nil {
		return err
	}
	switch info.State {
	case "Running":
		return nil
	case "Created":
		return m.apiPut(vmID, "vm.boot", nil)
	case "Paused":
		return m.apiPut(vmID, "vm.resume", nil)
	default:
		_ = m.apiPut(vmID, "vm.delete", nil)
		icfg, err := m.loadInstanceConfig(vmID)
		if err != nil {
			return err
		}
		netCfg, err := m.allocNetwork(vmID)
		if err != nil {
			return err
		}
		vmCfg, err := m.buildVmConfig(vmID, icfg, netCfg)
		if err != nil {
			return err
		}
		if err := m.apiPut(vmID, "vm.create", vmCfg); err != nil {
			return err
		}
		return m.apiPut(vmID, "vm.boot", nil)
	}
}

func (m *Manager) StopVM(_ context.Context, vmID string, force bool) error {
	if !m.isRunning(vmID) {
		return nil
	}
	if force {
		return m.killProcess(vmID)
	}
	if err := m.apiPut(vmID, "vm.power-button", nil); err != nil {
		_ = m.apiPut(vmID, "vm.shutdown", nil)
	}
	return nil
}

func (m *Manager) RestartVM(_ context.Context, vmID string) error {
	return m.apiPut(vmID, "vm.reboot", nil)
}

func (m *Manager) DeleteVM(_ context.Context, vmID string) error {
	_ = m.db.DeleteVMConfig(vmID)
	_ = m.db.DeleteCloudHVConfig(vmID)

	if m.isRunning(vmID) {
		_ = m.apiPut(vmID, "vm.shutdown", nil)
		time.Sleep(3 * time.Second)
		_ = m.killProcess(vmID)
	}

	netCfg, err := m.allocNetwork(vmID)
	if err == nil {
		_ = runCmd("ip", "link", "delete", netCfg.Tap)
		// 清理IPv6配置（如果使用了subnet模式）
		if netCfg.IPv6 != "" && m.ipv6Mode == "subnet" {
			// 删除neighbor proxy
			if outIf := defaultRouteIface(); outIf != "" {
				_ = runCmd("ip", "-6", "neigh", "del", "proxy", netCfg.IPv6, "dev", outIf)
			}
		}
	}

	for _, f := range []string{
		m.pidFile(vmID),
		m.sockPath(vmID),
		m.serialSockPath(vmID),
		filepath.Join(runDir, vmID+".log"),
	} {
		_ = os.Remove(f)
	}

	return os.RemoveAll(filepath.Join(m.instDir, vmID))
}

func (m *Manager) UpdateVM(ctx context.Context, vmID string, cpu int32, ramMB int64, diskGB int64, bandwidthMBPS int32) error {
	if !m.isRunning(vmID) {
		return fmt.Errorf("VM %q is not running", vmID)
	}

	icfg, err := m.loadInstanceConfig(vmID)
	if err != nil {
		return err
	}

	resizeReq := &vmResizeReq{}
	needResize := false

	if cpu > 0 {
		v := int(cpu)
		resizeReq.DesiredVcpus = &v
		icfg.CPU = v
		needResize = true
	}
	if ramMB > 0 {
		b := ramMB * 1024 * 1024
		resizeReq.DesiredRam = &b
		icfg.MemoryMB = ramMB
		needResize = true
	}

	if bandwidthMBPS > 0 {
		icfg.BandwidthMbps = int(bandwidthMBPS)
		nc, netErr := m.allocNetwork(vmID)
		if netErr == nil {
			_ = m.apiPut(vmID, "vm.remove-device", &vmRemoveDevice{ID: "net0"})
			newNet := netCfg{
				Tap:               nc.Tap,
				Mac:               nc.MAC,
				ID:                "net0",
				RateLimiterConfig: buildNetRateLimit(icfg.BandwidthMbps),
			}
			_ = m.apiPut(vmID, "vm.add-net", &newNet)
		}
	}

	_ = m.saveInstanceConfig(vmID, icfg)

	if !needResize {
		return nil
	}
	return m.apiPut(vmID, "vm.resize", resizeReq)
}

func (m *Manager) ReinstallVM(ctx context.Context, req *agent.CmdReinstallVM) error {
	_ = m.DeleteVM(ctx, req.VmId)
	return m.CreateVM(ctx, &agent.CmdCreateVM{
		VmId:         req.VmId,
		OsImage:      req.OsImage,
		RootPassword: req.RootPassword,
		Cpu:          req.Cpu,
		RamMb:        req.RamMb,
		DiskGb:       req.DiskGb,
	})
}

func (m *Manager) ResetPassword(ctx context.Context, vmID, password string) error {
	wasRunning := m.isRunning(vmID)
	if wasRunning {
		_ = m.StopVM(ctx, vmID, true)
		time.Sleep(2 * time.Second)
	}

	diskPath := filepath.Join(m.instDir, vmID, "system.raw")
	if err := offlineChpasswd(diskPath, password); err != nil {
		return err
	}

	if wasRunning {
		return m.StartVM(ctx, vmID)
	}
	return nil
}

func (m *Manager) GetVMInfo(_ context.Context, vmID string) (*agent.VMSummary, error) {
	ip, _ := m.GetVMLocalIP(nil, vmID)
	status := agent.VMStatus_VM_STATUS_STOPPED
	var in, out int64

	if m.isRunning(vmID) {
		if err := m.apiGet(vmID, "vm.info", nil); err == nil {
			status = agent.VMStatus_VM_STATUS_RUNNING
			if nc, err := m.allocNetwork(vmID); err == nil {
				in, out = m.getTraffic(nc.Tap)
			}
		}
	}

	return &agent.VMSummary{
		VmId:            vmID,
		Status:          status,
		Ips:             []string{ip},
		TrafficInBytes:  in,
		TrafficOutBytes: out,
	}, nil
}

func (m *Manager) ListVMs(_ context.Context) ([]*agent.VMSummary, error) {
	entries, err := os.ReadDir(m.instDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var result []*agent.VMSummary
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		vmID := e.Name()
		if _, err := m.loadInstanceConfig(vmID); err != nil {
			continue
		}

		ip, _ := m.GetVMLocalIP(nil, vmID)
		status := agent.VMStatus_VM_STATUS_STOPPED
		var in, out int64
		if m.isRunning(vmID) {
			if err := m.apiGet(vmID, "vm.info", nil); err == nil {
				status = agent.VMStatus_VM_STATUS_RUNNING
				if nc, err := m.allocNetwork(vmID); err == nil {
					in, out = m.getTraffic(nc.Tap)
				}
			}
		}
		result = append(result, &agent.VMSummary{
			VmId:            vmID,
			Status:          status,
			Ips:             []string{ip},
			TrafficInBytes:  in,
			TrafficOutBytes: out,
		})
	}
	return result, nil
}

func (m *Manager) GetVMLocalIP(_ context.Context, vmID string) (string, error) {
	netCfg, err := m.allocNetwork(vmID)
	if err != nil {
		return "", err
	}
	return netCfg.IPv4, nil
}

func (m *Manager) GetSupportedImages(_ context.Context) ([]*agent.OSImageInfo, error) {
	var images []*agent.OSImageInfo
	for _, distro := range []string{"debian", "alpine"} {
		if _, err := os.Stat(filepath.Join(imgDir, distro, "current.raw")); err == nil {
			name := strings.ToUpper(distro[:1]) + distro[1:] + " (cloud-hypervisor)"
			images = append(images, &agent.OSImageInfo{
				Id:   distro,
				Name: name,
			})
		}
	}
	if len(images) == 0 {
		return nil, fmt.Errorf("no VM images found in %s; run update-vm-images.sh first", imgDir)
	}
	return images, nil
}

func (m *Manager) AttachTTY(ctx context.Context, vmID string, stdin io.Reader, stdout io.Writer, resize <-chan manager.ResizeEvent) error {
	sockPath := m.serialSockPath(vmID)

	conn, err := net.DialTimeout("unix", sockPath, 10*time.Second)
	if err != nil {
		return fmt.Errorf("serial console unavailable (is VM running?): %w", err)
	}
	defer conn.Close()

	// 设置socket读写超时，防止挂起
	_ = conn.SetDeadline(time.Now().Add(1 * time.Hour))

	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	go func() {
		for range resize {
		}
	}()

	errc := make(chan error, 2)

	// 标准输出：从socket读取，写入stdout
	go func() {
		_, copyErr := io.Copy(stdout, conn)
		errc <- copyErr
	}()

	// 标准输入：从stdin读取，写入socket
	// 使用一个有缓冲的readers来避免io.Copy永远等待
	go func() {
		_, copyErr := io.Copy(conn, stdin)
		// 关闭socket的写端，这样VM会知道输入已结束
		if tcpConn, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = tcpConn.CloseWrite()
		}
		errc <- copyErr
	}()

	select {
	case <-ctx.Done():
		_ = conn.Close()
		return nil
	case err := <-errc:
		if ctx.Err() != nil {
			return nil
		}
		if err != nil && !isClosedError(err) {
			return err
		}
		return nil
	}
}

func isClosedError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "closed") || strings.Contains(s, "EOF")
}

// ─── 辅助函数 ─────────────────────────────────────────────────────────────────

func (m *Manager) killProcess(vmID string) error {
	data, err := os.ReadFile(m.pidFile(vmID))
	if err != nil {
		return nil
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	_ = proc.Signal(syscall.SIGTERM)
	time.Sleep(2 * time.Second)
	_ = proc.Kill()
	_ = os.Remove(m.pidFile(vmID))
	return nil
}

func (m *Manager) getTraffic(tap string) (in, out int64) {
	in = readInt64(fmt.Sprintf("/sys/class/net/%s/statistics/rx_bytes", tap))
	out = readInt64(fmt.Sprintf("/sys/class/net/%s/statistics/tx_bytes", tap))
	return
}

func readInt64(path string) int64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	val, _ := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	return val
}

func offlineChpasswd(diskPath, password string) error {
	for _, tool := range []string{"losetup", "mount", "umount", "chroot"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("tool %q not found; cannot reset password offline", tool)
		}
	}

	out, err := cmdOutput("losetup", "--find", "--show", "--partscan", diskPath)
	if err != nil {
		return fmt.Errorf("losetup: %w", err)
	}
	loopDev := strings.TrimSpace(out)
	defer func() { _ = runCmd("losetup", "-d", loopDev) }()

	rootPart, err := findRootPartition(loopDev)
	if err != nil {
		return fmt.Errorf("find root partition: %w", err)
	}

	mnt, err := os.MkdirTemp("", "ch-mnt-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(mnt)

	if err := runCmd("mount", rootPart, mnt); err != nil {
		return fmt.Errorf("mount %s: %w", rootPart, err)
	}
	defer func() { _ = runCmd("umount", mnt) }()

	chpasswdInput := fmt.Sprintf("root:%s\n", password)
	cmd := exec.Command("chroot", mnt, "bash", "-c", "echo '"+
		strings.ReplaceAll(chpasswdInput, "'", "'\\''")+"' | chpasswd")
	cmd.Stdin = strings.NewReader(chpasswdInput)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("chpasswd: %w: %s", err, out)
	}
	return nil
}

func findRootPartition(loopDev string) (string, error) {
	mnt, _ := os.MkdirTemp("", "ch-probe-")
	defer os.RemoveAll(mnt)
	if runCmd("mount", "-o", "ro", loopDev, mnt) == nil {
		_ = runCmd("umount", mnt)
		return loopDev, nil
	}

	for i := 1; i <= 4; i++ {
		part := fmt.Sprintf("%sp%d", loopDev, i)
		if _, err := os.Stat(part); err != nil {
			continue
		}
		if runCmd("mount", "-o", "ro", part, mnt) == nil {
			_ = runCmd("umount", mnt)
			return part, nil
		}
	}
	return "", fmt.Errorf("no mountable partition found on %s", loopDev)
}

func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func cmdOutput(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return string(out), err
}

func buildNetRateLimit(mbps int) *rateLimiterConfig {
	if mbps <= 0 {
		return nil
	}
	bytesPerSec := int64(mbps) * 1_000_000 / 8
	bucket := &tokenBucket{
		Size:         bytesPerSec,
		OneTimeBurst: bytesPerSec,
		RefillTime:   1000,
	}
	return &rateLimiterConfig{Bandwidth: bucket}
}
