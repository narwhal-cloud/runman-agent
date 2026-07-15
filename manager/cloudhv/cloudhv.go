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
	"runtime"
	"strconv"
	"strings"
	"sync"
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

// vmStatusInfo 虚拟机内部上报的状态信息
type vmStatusInfo struct {
	CpuPercent float32
	RamUsedMb  int64
	RamTotalMb int64
	Timestamp  time.Time
}

// Manager 实现 VMManager 接口，管理多个 cloud-hypervisor 进程（每个 VM 一个进程）。
type Manager struct {
	binary      string // cloud-hypervisor 二进制路径
	instDir     string // VM 实例数据目录 (imgDir/instances)
	db          *db.DB
	ipv6Mode    string                   // "none"/"subnet"/"snat"
	ipv6Subnet  string                   // 可用子网，如 "2001:db8:1234::/64"（mode=subnet）
	ipv6Addr    string                   // 单个公网 IPv6（mode=snat）
	ipv6Iface   string                   // IPv6 所在网卡
	ipv6Counter int                      // 子网模式下的地址计数器（用于分配 ::2, ::3 等）
	vmStatus    map[string]*vmStatusInfo // 虚拟机上报的状态信息（CPU+内存）
	vmStatusMu  sync.RWMutex
	mu          sync.Mutex // 全局锁，序列化所有虚拟机生命周期操作，防止 IP/TAP 分配冲突
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
	IoUring           bool               `json:"io_uring"`
	NumQueues         int                `json:"num_queues,omitempty"`
	Iommu             bool               `json:"iommu,omitempty"`
	RateLimiterConfig *rateLimiterConfig `json:"rate_limiter_config,omitempty"`
}

type netCfg struct {
	Tap               string             `json:"tap"`
	Mac               string             `json:"mac"`
	ID                string             `json:"id,omitempty"`
	NumQueues         int                `json:"num_queues,omitempty"`
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
// ipv6Mode, ipv6Subnet, ipv6Addr, ipv6Iface 从配置文件读取后由调用者传入。
func New(database *db.DB, ipv6Mode, ipv6Subnet, ipv6Addr, ipv6Iface string) (*Manager, error) {
	if _, err := os.Stat(chBinary); err != nil {
		return nil, fmt.Errorf("cloud-hypervisor binary not found at %q: %w", chBinary, err)
	}
	instDir := filepath.Join(imgDir, "instances")
	for _, d := range []string{instDir, runDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	m := &Manager{
		binary:      chBinary,
		instDir:     instDir,
		db:          database,
		ipv6Mode:    ipv6Mode,
		ipv6Subnet:  ipv6Subnet,
		ipv6Addr:    ipv6Addr,
		ipv6Iface:   ipv6Iface,
		ipv6Counter: 2, // 从 ::2 开始分配
		vmStatus:    make(map[string]*vmStatusInfo),
	}
	if m.ipv6Mode == "" {
		m.ipv6Mode = "none"
	}

	if err := m.ensureBridge(); err != nil {
		return nil, fmt.Errorf("setup bridge: %w", err)
	}
	return m, nil
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
	// 确保 FORWARD 链放行 bridge 流量 (IPv4) (无论出网卡是什么，网桥必须能转发)
	_ = runCmd("iptables", "-I", "FORWARD", "-i", bridge, "-j", "ACCEPT")
	_ = runCmd("iptables", "-I", "FORWARD", "-o", bridge, "-j", "ACCEPT")

	outIf := defaultRouteIface()
	if outIf != "" {
		cidr := netCIDRv4
		if runCmd("iptables", "-t", "nat", "-C", "POSTROUTING", "-s", cidr, "-o", outIf, "-j", "MASQUERADE") != nil {
			_ = runCmd("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", cidr, "-o", outIf, "-j", "MASQUERADE")
		}
	} else {
		log.Printf("WARNING: defaultRouteIface returned empty, IPv4 MASQUERADE may not be applied")
		// Fallback: apply MASQUERADE without specifying output interface
		cidr := netCIDRv4
		if runCmd("iptables", "-t", "nat", "-C", "POSTROUTING", "-s", cidr, "-j", "MASQUERADE") != nil {
			_ = runCmd("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", cidr, "-j", "MASQUERADE")
		}
	}
	_ = os.WriteFile("/proc/sys/net/ipv6/conf/all/forwarding", []byte("1"), 0644)
	_ = os.WriteFile("/proc/sys/net/ipv6/conf/all/proxy_ndp", []byte("1"), 0644)
	_ = os.WriteFile("/proc/sys/net/ipv6/conf/default/forwarding", []byte("1"), 0644)
	_ = os.WriteFile("/proc/sys/net/ipv6/conf/default/proxy_ndp", []byte("1"), 0644)

	outIf = defaultRouteIface()
	if outIf != "" {
		_ = os.WriteFile(fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/forwarding", outIf), []byte("1"), 0644)
		_ = os.WriteFile(fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/proxy_ndp", outIf), []byte("1"), 0644)
	}

	// 确保 FORWARD 链放行 bridge 流量 (IPv6)
	_ = runCmd("ip6tables", "-I", "FORWARD", "-i", bridge, "-j", "ACCEPT")
	_ = runCmd("ip6tables", "-I", "FORWARD", "-o", bridge, "-j", "ACCEPT")

	// 7. IPv6 SNAT（如果是 SNAT 模式）
	if m.ipv6Mode == "snat" && m.ipv6Iface != "" {
		// 7a. 给网桥添加 ULA IPv6 地址（作为内网网关）
		_ = runCmd("ip", "-6", "addr", "add", gwIPv6Local+"/64", "dev", bridge)

		// 7b. 配置 IPv6 MASQUERADE
		// ULA 网段出站（如果源地址是 ULA，SNAT 到宿主机 IPv6）
		cidr6 := netCIDRv6Local
		if runCmd("ip6tables", "-t", "nat", "-C", "POSTROUTING", "-s", cidr6, "-o", m.ipv6Iface, "-j", "MASQUERADE") != nil {
			_ = runCmd("ip6tables", "-t", "nat", "-A", "POSTROUTING", "-s", cidr6, "-o", m.ipv6Iface, "-j", "MASQUERADE")
		}

		// 7c. 配置宿主机 IPv6 代理 NDP（使宿主机的 IPv6 地址可通过网桥访问）
		if m.ipv6Addr != "" {
			_ = runCmd("ip", "neigh", "add", "proxy", m.ipv6Addr, "dev", bridge)
		}

		log.Printf("info: SNAT mode IPv6 configured: gateway=%s, host=%s", gwIPv6Local+"/64", m.ipv6Addr)
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
		log.Printf("[allocNetwork] error: failed to allocate idx: %v", err)
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
		log.Printf("[allocNetwork] error: failed to save config: %v", err)
		return nil, fmt.Errorf("save network config: %w", err)
	}
	log.Printf("[allocNetwork] allocated new network: vmID=%s idx=%d tap=vm%d ipv4=%s ipv6=%s", vmID, idx, idx, ipv4, ipv6)

	return m.netConfigFromDB(conf), nil
}

// getNetworkConfig is the read-only counterpart of allocNetwork.
// It only reads from the database; it never allocates a new index or creates a record.
// Use this in informational paths (ListVMs, GetVMInfo, GetVMNetStats, deleteVM tap cleanup)
// to avoid accidentally persisting a CloudHVVMConfig that was never intentionally created.
func (m *Manager) getNetworkConfig(vmID string) (*netConfig, error) {
	conf, err := m.db.GetCloudHVConfig(vmID)
	if err != nil {
		return nil, fmt.Errorf("no network config for VM %s: %w", vmID, err)
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
		if m.ipv6Subnet != "" {
			return m.allocIPv6FromSubnet(idx)
		}
		return fmt.Sprintf("fd91:cafe:cafe:10::%d", idx)
	case "snat":
		// SNAT 模式：分配 ULA 私有地址，后续通过宿主机 ip6tables 进行 MASQUERADE
		return fmt.Sprintf("fd91:cafe:cafe:10::%d", idx)
	default:
		// 默认：使用 ULA 段（即使 mode=none 或 mode=""）
		return fmt.Sprintf("fd91:cafe:cafe:10::%d", idx)
	}
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

func (m *Manager) setupNetwork(net *netConfig, cpu int) error {
	// 清理旧的 TAP 设备。避免被遗留的 vhost 占用，或者之前创建时没有带 multi_queue 参数，
	// 导致后续 CPU > 1 的 VM 启动时内核返回 Resource busy (os error 16)。
	if runCmd("ip", "link", "show", net.Tap) == nil {
		log.Printf("[setupNetwork] removing old TAP device: %s", net.Tap)
		_ = runCmd("ip", "link", "delete", net.Tap)
	}

	// CPU > 1 时才开启 multi_queue：cloud-hypervisor 在 num_queues=2（单队列）时打开 TAP
	// 不带 IFF_MULTI_QUEUE，而 multi_queue TAP 要求所有 FD 都带该 flag，否则内核报错。
	tapArgs := []string{"tuntap", "add", net.Tap, "mode", "tap"}
	if cpu > 1 {
		tapArgs = append(tapArgs, "multi_queue")
	}
	log.Printf("[setupNetwork] creating TAP device (multi_queue=%v): %s", cpu > 1, net.Tap)
	if err := runCmd("ip", tapArgs...); err != nil {
		log.Printf("[setupNetwork] error: failed to create TAP device %s: %v", net.Tap, err)
		return fmt.Errorf("create tap %s: %w", net.Tap, err)
	}
	log.Printf("[setupNetwork] TAP device created: %s", net.Tap)

	// 加入 bridge 并启动
	if err := runCmd("ip", "link", "set", net.Tap, "master", bridge); err != nil {
		log.Printf("[setupNetwork] error: failed to add TAP to bridge: %v", err)
	}
	if err := runCmd("ip", "link", "set", net.Tap, "up"); err != nil {
		log.Printf("[setupNetwork] error: failed to bring TAP up: %v", err)
	}
	log.Printf("[setupNetwork] TAP added to bridge and brought up: %s", net.Tap)

	// 开启 TAP 设备的 proxy_ndp，帮助 VM 和外部跨网桥顺畅沟通
	proxyNdpPath := fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/proxy_ndp", net.Tap)
	if err := os.WriteFile(proxyNdpPath, []byte("1"), 0644); err != nil {
		log.Printf("[setupNetwork] warning: failed to enable proxy_ndp on %s: %v", net.Tap, err)
	}
	forwardingPath := fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/forwarding", net.Tap)
	if err := os.WriteFile(forwardingPath, []byte("1"), 0644); err != nil {
		log.Printf("[setupNetwork] warning: failed to enable forwarding on %s: %v", net.Tap, err)
	}

	// IPv6 模式特定配置
	if net.IPv6 != "" {
		switch m.ipv6Mode {
		case "subnet":
			// 依赖于 runman-agent 内置的 ndp responder (ndp/responder.go) 处理 NDP 请求
			// 不再依赖内核的 proxy_ndp (ip neigh add proxy)，因为内核要求严苛的路由匹配且容易失效。
		case "snat":
			// SNAT 配置在 ensureBridge 中做一次就够了
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

	// 使用 qemu-img convert 确保目标是一个规整的 raw 文件
	if err := runCmd("qemu-img", "convert", "-f", "raw", "-O", "raw", base, system); err != nil {
		return fmt.Errorf("convert base image: %w", err)
	}

	if diskGB > 0 {
		if err := runCmd("qemu-img", "resize", "-f", "raw", system, fmt.Sprintf("%dG", diskGB)); err != nil {
			return fmt.Errorf("resize disk: %w", err)
		}
		// 在宿主机端扩展根分区，使其填满磁盘
		if err := expandRootPartition(system); err != nil {
			log.Printf("[prepareDisks] warning: expand root partition failed: %v", err)
		}
	}
	return nil
}

// expandRootPartition 使用 sfdisk 扩展 raw 磁盘镜像中的分区 1（根分区）。
// sfdisk 是 util-linux 的一部分，无需额外安装。
// ", +" 表示保持起始扇区不变，扩展到最大可用空间。
// sfdisk 写入时会自动修复 GPT 备份头。
func expandRootPartition(diskPath string) error {
	cmd := exec.Command("sfdisk", "-N", "1", "--force", diskPath)
	cmd.Stdin = strings.NewReader(", +\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("sfdisk expand partition 1: %s: %w", strings.TrimSpace(string(out)), err)
	}
	log.Printf("[expandRootPartition] success: %s", diskPath)
	return nil
}

// ─── Cloud-init ────────────────────────────────────────────────────────────────

func (m *Manager) genCloudInit(vmID, distro, password string, net *netConfig) error {
	dir := filepath.Join(m.instDir, vmID)
	ciDir := filepath.Join(dir, "cloudinit")
	_ = os.MkdirAll(ciDir, 0755)

	// 必须严格遵守 YAML 缩进格式
	// 基础镜像已预装 curl/e2fsprogs 等软件包并配置好 sshd root 登录
	// （images 仓库 CI 预构建或 install.sh 回退构建时烤入），
	// 实例级 cloud-init 不再执行联网装包，减少开机时间；
	// runcmd 仅保留毫秒级本地命令（sed/重启 sshd）兼容旧版 install.sh 构建的镜像
	var svcFile, svcEnable string
	if distro == "alpine" {
		svcFile = `  - path: /etc/init.d/report-memory
    content: |
      #!/sbin/openrc-run
      description="Report VM memory usage to agent"
      command="/usr/local/bin/report-memory.sh"
      command_background=true
      pidfile="/var/run/report-memory.pid"
    permissions: '0755'`
		svcEnable = `  - rc-update add report-memory default
  - rc-service report-memory start`
	} else {
		svcFile = `  - path: /etc/systemd/system/report-memory.service
    content: |
      [Unit]
      Description=Report VM memory usage to agent
      After=network.target
 
      [Service]
      Type=simple
      ExecStart=/usr/local/bin/report-memory.sh
      Restart=always
      RestartSec=10
      StandardOutput=journal
      StandardError=journal
 
      [Install]
      WantedBy=multi-user.target
    permissions: '0644'`
		svcEnable = `  - systemctl daemon-reload
  - systemctl enable report-memory.service
  - systemctl start report-memory.service`
	}

	userData := fmt.Sprintf(`#cloud-config
ssh_pwauth: true
chpasswd:
  list: |
    root:%s
  expire: false
users:
  - name: root
    ssh_pwauth: true
write_files:
  - path: /etc/resolv.conf
    content: |
      nameserver 1.1.1.1
      nameserver 2606:4700:4700::1111
    permissions: '0644'
  - path: /usr/local/bin/report-memory.sh
    content: |
      #!/bin/sh
      VMID="%s"
 
      while true; do
        MEM_USED=$(free -m | awk 'NR==2 {print $3}')
        MEM_TOTAL=$(free -m | awk 'NR==2 {print $2}')

        read -r cpu user nice system idle iowait irq softirq steal guest guest_nice < /proc/stat
        t1=$((user+nice+system+idle+iowait+irq+softirq+steal))
        i1=$((idle+iowait))

        sleep 1

        read -r cpu user nice system idle iowait irq softirq steal guest guest_nice < /proc/stat
        t2=$((user+nice+system+idle+iowait+irq+softirq+steal))
        i2=$((idle+iowait))

        diff_t=$((t2-t1))
        diff_i=$((i2-i1))

        if [ "$diff_t" -le 0 ]; then
          CPU_PERCENT=0
        else
          CPU_PERCENT=$(((diff_t - diff_i) * 100 / diff_t))
        fi

        PAYLOAD="{\"vm_id\":\"$VMID\",\"cpu_percent\":$CPU_PERCENT,\"ram_used_mb\":$MEM_USED,\"ram_total_mb\":$MEM_TOTAL}"
        echo "Sending: $PAYLOAD"

        curl -s -o /dev/null -X POST http://10.91.0.1:8792/api/vm-status \
          -H "Content-Type: application/json" -d "$PAYLOAD"

        sleep 30
      done
    permissions: '0755'
%s
runcmd:
  - resize2fs /dev/vda1 || true
  - sed -i 's/^#\?PermitRootLogin.*/PermitRootLogin yes/' /etc/ssh/sshd_config
  - [ sh, -c, "systemctl restart sshd 2>/dev/null || rc-service sshd restart 2>/dev/null || true" ]
%s
`, password, vmID, svcFile, svcEnable)

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

	if m.ipv6Mode != "none" && net.IPv6 != "" {
		log.Printf("[CloudHV] VM %s IPv6 config: %s (mode=%s)", vmID, net.IPv6, m.ipv6Mode)
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
	dir := filepath.Join(m.instDir, vmID)
	// 确保目录存在
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, data, 0644); err != nil {
		return fmt.Errorf("write config %s: %w", cfgPath, err)
	}
	return nil
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
		memBytes = 256 * 1024 * 1024
	}

	// 内存 <512M 时，通过 hotplug_size 在启动时动态分配额外内存
	var hotplugSize *int64
	if icfg.MemoryMB > 0 && icfg.MemoryMB < 512 {
		// virtio-mem 要求对齐到 128MB 的倍数
		const virtioMemAlignment = int64(128 * 1024 * 1024) // 128MB
		doubledMemBytes := icfg.MemoryMB * 2 * 1024 * 1024
		// 向上对齐到 128MB 倍数
		doubledMemBytes = ((doubledMemBytes + virtioMemAlignment - 1) / virtioMemAlignment) * virtioMemAlignment
		hotplugSize = &doubledMemBytes
	}

	disks := []diskConfig{
		{
			Path:              filepath.Join(m.instDir, vmID, "system.raw"),
			NumQueues:         1,
			Direct:            false,
			IoUring:           true,
			RateLimiterConfig: buildDiskRateLimit(icfg.BandwidthMbps),
		},
	}
	if _, err := os.Stat(filepath.Join(m.instDir, vmID, "cloudinit.img")); err == nil {
		disks = append(disks, diskConfig{
			Path:      filepath.Join(m.instDir, vmID, "cloudinit.img"),
			Readonly:  true,
			Direct:    false,
			IoUring:   false,
			NumQueues: 1,
		})
	}

	return &vmCreateReq{
		Cpus: cpusConfig{
			BootVcpus: cpu,
			MaxVcpus:  runtime.NumCPU(),
		},
		Memory: memoryConfig{
			Size:          memBytes,
			Mergeable:     true,
			Thp:           true,
			HotplugMethod: "VirtioMem",
			HotplugSize:   hotplugSize,
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
			NumQueues:         icfg.CPU * 2, // 必须显式告诉 cloud-hypervisor 使用几个队列，通常建议为 vCPU * 2（RX/TX）
			RateLimiterConfig: buildNetRateLimit(icfg.BandwidthMbps),
		}},
		Rng: &rngConfig{Src: "/dev/urandom"},
		Balloon: func() *balloonConfig {
			// 内存 < 512M 时禁用 balloon 以避免 OOM
			if icfg.MemoryMB > 0 && icfg.MemoryMB < 512 {
				return nil
			}
			return &balloonConfig{
				Size:              0,
				DeflateOnOom:      true,
				FreePageReporting: true,
			}
		}(),
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
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.createVM(ctx, req)
}

func (m *Manager) createVM(_ context.Context, req *agent.CmdCreateVM) error {
	log.Printf("[CreateVM] start: vmID=%s image=%s cpu=%d ramMb=%d diskGb=%d", req.VmId, req.OsImage, req.Cpu, req.RamMb, req.DiskGb)

	// 限制最低配置：CPU 1核，内存 256MB，磁盘 3GB
	if req.Cpu < 1 {
		req.Cpu = 1
	}
	if req.RamMb < 256 {
		req.RamMb = 256
	}
	if req.DiskGb < 3 {
		req.DiskGb = 3
	}

	dir := filepath.Join(m.instDir, req.VmId)
	// 确保目录干净（重装时删除旧目录）
	_ = os.RemoveAll(dir)
	log.Printf("[CreateVM] directory cleaned: %s", dir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("[CreateVM] error: failed to create directory: %v", err)
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
		log.Printf("[CreateVM] error: failed to save config: %v", err)
		return err
	}
	log.Printf("[CreateVM] config saved: vmID=%s", req.VmId)

	srcCmdline := filepath.Join(imgDir, req.OsImage, "cmdline")
	cmdlineData, readErr := os.ReadFile(srcCmdline)
	if readErr != nil {
		log.Printf("[CreateVM] error: cmdline not found for distro %q: %v", req.OsImage, readErr)
		return fmt.Errorf("cmdline not found for distro %q: %w", req.OsImage, readErr)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), cmdlineData, 0644); err != nil {
		log.Printf("[CreateVM] error: failed to write cmdline: %v", err)
		return err
	}
	log.Printf("[CreateVM] cmdline written: %s", req.OsImage)

	netCfg, err := m.allocNetwork(req.VmId)
	if err != nil {
		log.Printf("[CreateVM] error: failed to allocate network: %v", err)
		return err
	}
	log.Printf("[CreateVM] network allocated: tap=%s ipv4=%s ipv6=%s", netCfg.Tap, netCfg.IPv4, netCfg.IPv6)

	if err := m.prepareDisks(req.VmId, req.OsImage, req.DiskGb); err != nil {
		log.Printf("[CreateVM] error: failed to prepare disks: %v", err)
		return err
	}
	log.Printf("[CreateVM] disks prepared: vmID=%s", req.VmId)

	if err := m.genCloudInit(req.VmId, req.OsImage, req.RootPassword, netCfg); err != nil {
		log.Printf("[CreateVM] error: failed to generate cloud-init: %v", err)
		return err
	}
	log.Printf("[CreateVM] cloud-init generated: vmID=%s", req.VmId)

	if err := m.setupNetwork(netCfg, int(req.Cpu)); err != nil {
		log.Printf("[CreateVM] error: failed to setup network: %v", err)
		return err
	}
	log.Printf("[CreateVM] network setup completed: tap=%s", netCfg.Tap)

	if err := m.launchProcess(req.VmId); err != nil {
		log.Printf("[CreateVM] error: failed to launch process: %v", err)
		return err
	}
	log.Printf("[CreateVM] process launched: vmID=%s", req.VmId)

	vmCfg, err := m.buildVmConfig(req.VmId, icfg, netCfg)
	if err != nil {
		log.Printf("[CreateVM] error: failed to build vm config: %v", err)
		return err
	}
	if err := m.apiPut(req.VmId, "vm.create", vmCfg); err != nil {
		log.Printf("[CreateVM] error: vm.create API failed: %v", err)
		return err
	}
	log.Printf("[CreateVM] vm.create API called: vmID=%s", req.VmId)

	if err := m.apiPut(req.VmId, "vm.boot", nil); err != nil {
		log.Printf("[CreateVM] error: vm.boot API failed: %v", err)
		return err
	}
	log.Printf("[CreateVM] vm.boot API called: vmID=%s", req.VmId)

	// 保存业务层通用配置到数据库
	bizConf, _ := m.db.GetVMConfig(req.VmId)
	if bizConf == nil {
		bizConf = &db.VMConfig{VMID: req.VmId}
	}
	bizConf.CPU = int(req.Cpu)
	bizConf.MemoryMB = req.RamMb
	bizConf.DiskGB = req.DiskGb
	bizConf.Image = req.OsImage
	bizConf.Status = "running"
	if err := m.db.SaveVMConfig(bizConf); err != nil {
		log.Printf("[CreateVM] warning: SaveVMConfig failed: %v", err)
	}
	log.Printf("[CreateVM] VMConfig saved: vmID=%s cpu=%d ram=%dMB disk=%dGB image=%s",
		req.VmId, req.Cpu, req.RamMb, req.DiskGb, req.OsImage)

	log.Printf("[CreateVM] success: vmID=%s", req.VmId)
	return nil
}

func (m *Manager) StartVM(ctx context.Context, vmID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startVM(ctx, vmID)
}

func (m *Manager) startVM(_ context.Context, vmID string) error {
	if !m.isRunning(vmID) {
		icfg, err := m.loadInstanceConfig(vmID)
		if err != nil {
			return fmt.Errorf("instance %q not found: %w", vmID, err)
		}
		netCfg, err := m.allocNetwork(vmID)
		if err != nil {
			return err
		}
		if err := m.setupNetwork(netCfg, icfg.CPU); err != nil {
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

func (m *Manager) StopVM(ctx context.Context, vmID string, force bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopVM(ctx, vmID, force)
}

func (m *Manager) stopVM(_ context.Context, vmID string, force bool) error {
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
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.apiPut(vmID, "vm.reboot", nil)
}

func (m *Manager) DeleteVM(ctx context.Context, vmID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.deleteVM(ctx, vmID)
}

func (m *Manager) deleteVM(_ context.Context, vmID string) error {
	log.Printf("[DeleteVM] vmID=%s", vmID)

	if m.isRunning(vmID) {
		log.Printf("[DeleteVM] stopping running VM: %s", vmID)
		_ = m.apiPut(vmID, "vm.shutdown", nil)
		time.Sleep(3 * time.Second)
		_ = m.killProcess(vmID)
		log.Printf("[DeleteVM] VM stopped: %s", vmID)
	}

	// 获取网络配置（只读查询，不分配新记录）
	if netCfg, err := m.getNetworkConfig(vmID); err == nil {
		log.Printf("[DeleteVM] deleting TAP device: %s", netCfg.Tap)
		if err := runCmd("ip", "link", "delete", netCfg.Tap); err != nil {
			log.Printf("[DeleteVM] warning: failed to delete TAP device: %v", err)
		}
	}

	// 清理运行时文件
	for _, f := range []string{
		m.pidFile(vmID),
		m.sockPath(vmID),
		m.serialSockPath(vmID),
		filepath.Join(runDir, vmID+".log"),
	} {
		_ = os.Remove(f)
	}

	// 删除 DB 记录（先于磁盘删除，确保二者结果一致：DB 失败则整体失败，磁盘不会被删）
	if err := m.db.DeleteVMConfig(vmID); err != nil {
		log.Printf("[DeleteVM] error: failed to delete VMConfig for %s: %v", vmID, err)
		return fmt.Errorf("delete VMConfig: %w", err)
	}
	if err := m.db.DeleteCloudHVConfig(vmID); err != nil {
		log.Printf("[DeleteVM] warning: failed to delete CloudHVConfig for %s: %v", vmID, err)
	}
	log.Printf("[DeleteVM] database records deleted: %s", vmID)

	// 删除实例目录
	instPath := filepath.Join(m.instDir, vmID)
	if err := os.RemoveAll(instPath); err != nil {
		log.Printf("[DeleteVM] error: failed to remove instance directory %q: %v", instPath, err)
		return err
	}
	log.Printf("[DeleteVM] success: instance directory removed: %s", vmID)
	return nil
}

func (m *Manager) ReinstallVM(ctx context.Context, req *agent.CmdReinstallVM) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	log.Printf("[ReinstallVM] start: vmID=%s image=%s", req.VmId, req.OsImage)
	if err := m.deleteVM(ctx, req.VmId); err != nil {
		log.Printf("[ReinstallVM] warning: DeleteVM returned error: %v", err)
		// 继续重建，不因为删除失败而中止
	}
	log.Printf("[ReinstallVM] deleted old VM, now creating new one: vmID=%s", req.VmId)
	// CreateVM 会先删除旧目录再重建，确保干净的重装
	return m.createVM(ctx, &agent.CmdCreateVM{
		VmId:         req.VmId,
		OsImage:      req.OsImage,
		RootPassword: req.RootPassword,
		Cpu:          req.Cpu,
		RamMb:        req.RamMb,
		DiskGb:       req.DiskGb,
	})
}

func (m *Manager) ResetPassword(ctx context.Context, vmID, password string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	wasRunning := m.isRunning(vmID)
	if wasRunning {
		_ = m.stopVM(ctx, vmID, true)
		time.Sleep(2 * time.Second)
	}

	diskPath := filepath.Join(m.instDir, vmID, "system.raw")
	if err := offlineChpasswd(diskPath, password); err != nil {
		return err
	}

	if wasRunning {
		return m.startVM(ctx, vmID)
	}
	return nil
}

func (m *Manager) GetVMInfo(_ context.Context, vmID string) (*agent.VMSummary, error) {
	var ips []string
	nc, ncErr := m.getNetworkConfig(vmID)
	if ncErr == nil {
		if nc.IPv4 != "" {
			ips = append(ips, nc.IPv4)
		}
		if nc.IPv6 != "" && m.ipv6Mode != "none" {
			ips = append(ips, nc.IPv6)
		}
	}

	status := agent.VMStatus_VM_STATUS_STOPPED
	var in, out, memUsed int64
	var cpuPct float32

	if m.isRunning(vmID) {
		if err := m.apiGet(vmID, "vm.info", nil); err == nil {
			status = agent.VMStatus_VM_STATUS_RUNNING
			if ncErr == nil {
				in, out = m.getTraffic(nc.Tap)
			}
			cpuPct, memUsed = m.getUsage(vmID)
		}
	}

	return &agent.VMSummary{
		VmId:            vmID,
		Status:          status,
		CpuPct:          cpuPct,
		RamUsedMb:       memUsed,
		Ips:             ips,
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
			log.Printf("[ListVMs] WARNING: failed to load config for %s: %v", vmID, err)
			continue
		}

		var ips []string
		nc, ncErr := m.getNetworkConfig(vmID)
		if ncErr == nil {
			if nc.IPv4 != "" {
				ips = append(ips, nc.IPv4)
			}
			if nc.IPv6 != "" && m.ipv6Mode != "none" {
				ips = append(ips, nc.IPv6)
			}
		}

		status := agent.VMStatus_VM_STATUS_STOPPED
		var in, out, memUsed int64
		var cpuPct float32
		if m.isRunning(vmID) {
			if err := m.apiGet(vmID, "vm.info", nil); err == nil {
				status = agent.VMStatus_VM_STATUS_RUNNING
				if ncErr == nil {
					in, out = m.getTraffic(nc.Tap)
				}
				cpuPct, memUsed = m.getUsage(vmID)
			}
		}
		result = append(result, &agent.VMSummary{
			VmId:            vmID,
			Status:          status,
			CpuPct:          cpuPct,
			RamUsedMb:       memUsed,
			Ips:             ips,
			TrafficInBytes:  in,
			TrafficOutBytes: out,
		})
	}
	return result, nil
}

func (m *Manager) GetVMLocalIP(_ context.Context, vmID string) (string, error) {
	netCfg, err := m.getNetworkConfig(vmID)
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
		return nil, fmt.Errorf("no VM images found in %s; run install.sh to download base images", imgDir)
	}
	return images, nil
}

func (m *Manager) AttachTTY(ctx context.Context, vmID string, stdin io.Reader, stdout io.Writer, resize <-chan manager.ResizeEvent) error {
	sockPath := m.serialSockPath(vmID)

	conn, err := net.DialTimeout("unix", sockPath, 10*time.Second)
	if err != nil {
		return fmt.Errorf("serial console unavailable (is VM running?): %w", err)
	}
	defer func() { _ = conn.Close() }()

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

func (m *Manager) getUsage(vmID string) (cpuPct float32, ramUsedMb int64) {
	// 获取虚拟机上报的状态信息（CPU + 内存，或配置值）
	cpuPct, ramUsedMb, _ = m.GetVMStatus(vmID)
	return
}

func (m *Manager) getTraffic(tap string) (in, out int64) {
	// 从宿主机视角看：
	// TX 是宿主机发往 TAP 的流量 = 虚拟机的入站流量 (In)
	// RX 是宿主机从 TAP 接收的流量 = 虚拟机的出站流量 (Out)
	out = readInt64(fmt.Sprintf("/sys/class/net/%s/statistics/rx_bytes", tap))
	in = readInt64(fmt.Sprintf("/sys/class/net/%s/statistics/tx_bytes", tap))
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
	defer func() { _ = os.RemoveAll(mnt) }()

	if err = runCmd("mount", rootPart, mnt); err != nil {
		return fmt.Errorf("mount %s: %w", rootPart, err)
	}
	defer func() { _ = runCmd("umount", mnt) }()

	chpasswdInput := fmt.Sprintf("root:%s\n", password)
	cmd := exec.Command("chroot", mnt, "chpasswd")
	cmd.Stdin = strings.NewReader(chpasswdInput)
	if out, err := cmd.CombinedOutput(); err != nil {
		// 备选方案：如果 chpasswd 不在 PATH 中，尝试使用 sh -c
		if strings.Contains(err.Error(), "not found") || strings.Contains(string(out), "not found") {
			cmd = exec.Command("chroot", mnt, "sh", "-c", "chpasswd")
			cmd.Stdin = strings.NewReader(chpasswdInput)
			out, err = cmd.CombinedOutput()
			if err != nil {
				return fmt.Errorf("chpasswd (fallback): %w: %s", err, out)
			}
			return nil
		}
		return fmt.Errorf("chpasswd: %w: %s", err, out)
	}
	return nil
}

func findRootPartition(loopDev string) (string, error) {
	mnt, _ := os.MkdirTemp("", "ch-probe-")
	defer func() { _ = os.RemoveAll(mnt) }()
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

// GetVMNetStats 获取 VM 的 network 流量统计（用于流量统计服务）
func (m *Manager) GetVMNetStats(_ context.Context, vmID string) (*manager.VMNetStats, error) {
	nc, err := m.getNetworkConfig(vmID)
	if err != nil {
		return nil, err
	}
	in, out := m.getTraffic(nc.Tap)
	return &manager.VMNetStats{
		VMID:     vmID,
		InBytes:  in,
		OutBytes: out,
	}, nil
}

func (m *Manager) Cleanup(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	entries, err := os.ReadDir(m.instDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	// 获取数据库中登记的所有虚拟机，DB 出错时必须中止：
	// 空列表会把所有实例误判为幽灵实例并删除磁盘数据。
	configs, err := m.db.ListVMConfigs()
	if err != nil {
		log.Printf("[CloudHV] Cleanup: failed to list VM configs, skipping to avoid false deletions: %v", err)
		return err
	}
	registered := make(map[string]bool, len(configs))
	for _, c := range configs {
		registered[c.VMID] = true
	}

	// 统计实例目录数量（仅目录）
	var dirCount int
	for _, e := range entries {
		if e.IsDir() {
			dirCount++
		}
	}
	// 安全检查：DB 为空但磁盘有实例目录，放弃本次清理。
	if len(configs) == 0 && dirCount > 0 {
		log.Printf("[CloudHV] Cleanup: DB has 0 configs but %d instance dirs exist, skipping to avoid false deletions", dirCount)
		return nil
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		vmID := e.Name()

		// 跳过已登记的
		if registered[vmID] {
			continue
		}

		// 判定为幽灵实例，直接删除
		log.Printf("[CloudHV] Found ghost instance %s, deleting...", vmID)
		_ = m.deleteVM(ctx, vmID)
	}
	return nil
}

func cmdOutput(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return string(out), err
}

func buildDiskRateLimit(mbps int) *rateLimiterConfig {
	// 磁盘速率最小100mbps
	if mbps <= 0 {
		mbps = 100
	}
	bytesPerSec := int64(mbps) * 1_000_000 / 8
	bucket := &tokenBucket{
		Size:         bytesPerSec,
		OneTimeBurst: bytesPerSec,
		RefillTime:   1000,
	}
	return &rateLimiterConfig{Bandwidth: bucket}
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

// UpdateVMMemory 接收虚拟机上报的内存信息
// 路由: POST /api/vm-memory
func (m *Manager) UpdateVMStatus(vmID string, cpuPercent float32, usedMb, totalMb int64) {
	m.vmStatusMu.Lock()
	defer m.vmStatusMu.Unlock()
	m.vmStatus[vmID] = &vmStatusInfo{
		CpuPercent: cpuPercent,
		RamUsedMb:  usedMb,
		RamTotalMb: totalMb,
		Timestamp:  time.Now(),
	}
}

// GetVMStatus 获取虚拟机的状态信息（CPU + 内存，从虚拟机上报或配置）
func (m *Manager) GetVMStatus(vmID string) (cpuPercent float32, usedMb, totalMb int64) {
	m.vmStatusMu.RLock()
	status, exists := m.vmStatus[vmID]
	m.vmStatusMu.RUnlock()

	if exists && time.Since(status.Timestamp) < 5*time.Minute {
		// 使用虚拟机上报的数据（5分钟内有效）
		return status.CpuPercent, status.RamUsedMb, status.RamTotalMb
	}

	// 返回虚拟机配置的内存值（作为总量）
	if icfg, err := m.loadInstanceConfig(vmID); err == nil {
		return 0, 0, int64(icfg.MemoryMB)
	}
	return 0, 0, 0
}
