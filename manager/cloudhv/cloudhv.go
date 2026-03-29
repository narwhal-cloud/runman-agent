package cloudhv

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"runman-agent/manager"
	"runman-agent/proto/agent"
)

const (
	imgDir    = "/opt/vm-images"
	runDir    = "/run/cloud-hypervisor"
	gwIP      = "192.168.249.1"
	netPrefix = "192.168.249"
	bridge    = "br-vms"
	tapBase   = "vmtap"
)

// Manager 实现 VMManager 接口，管理多个 cloud-hypervisor 进程（每个 VM 一个进程）。
type Manager struct {
	binary  string // cloud-hypervisor 二进制路径
	instDir string // VM 实例数据目录 (imgDir/instances)
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

// cpusConfig 对应 cloud-hypervisor CpusConfig。
// MaxVcpus 设为 64，预留热插拔空间，实际使用 BootVcpus 个核心。
// cloud-hypervisor 直接向 guest 呈现 boot_vcpus 个虚拟 CPU，无需 cpuset 亲和绑定。
type cpusConfig struct {
	BootVcpus int `json:"boot_vcpus"`
	MaxVcpus  int `json:"max_vcpus"`
}

// memoryConfig 对应 cloud-hypervisor MemoryConfig。
// HotplugMethod 使用 "VirtioMem" 以支持内存双向热调整（ACPI 只能加不能减）。
// HotplugSize 预留虚拟地址空间，不占用实际物理内存，设为 64 GB 作为上限。
// Mergeable 启用 KSM 内核同页合并，提升多 VM 共存时的内存利用率。
// Thp 启用透明大页，减少虚拟化缺页中断（默认已开启，此处显式声明）。
type memoryConfig struct {
	Size          int64  `json:"size"`
	Mergeable     bool   `json:"mergeable"`
	Thp           bool   `json:"thp"`
	HotplugMethod string `json:"hotplug_method,omitempty"`
	HotplugSize   *int64 `json:"hotplug_size,omitempty"`
}

// balloonConfig 对应 cloud-hypervisor BalloonConfig。
// DeflateOnOom 在 guest OOM 时自动缩减气球，避免 guest 因内存压力崩溃。
// FreePageReporting 让 guest 向宿主机上报空闲页，宿主机可将其归还给物理内存池。
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

// diskConfig 对应 cloud-hypervisor DiskConfig。
// NumQueues 多队列 I/O 提升 SSD 并发吞吐；随机读写密集型负载收益明显。
// 注意：系统盘使用稀疏文件（cp --sparse=always），不设置 Direct（O_DIRECT），
// 避免向稀疏文件空洞写入时因内核需异步分配块而导致写失败。
// RateLimiterConfig 磁盘限速，留空表示不限制。
type diskConfig struct {
	Path              string             `json:"path"`
	Readonly          bool               `json:"readonly,omitempty"`
	Direct            bool               `json:"direct,omitempty"`
	NumQueues         int                `json:"num_queues,omitempty"`
	RateLimiterConfig *rateLimiterConfig `json:"rate_limiter_config,omitempty"`
}

// netCfg 对应 cloud-hypervisor NetConfig。
// ID 固定为 "net0"，供热替换时通过 vm.remove-device 定位设备。
// RateLimiterConfig 在 virtio 层限速，覆盖所有 VM 流量（包含未转发的端口）。
type netCfg struct {
	Tap               string             `json:"tap"`
	Mac               string             `json:"mac"`
	ID                string             `json:"id,omitempty"`
	RateLimiterConfig *rateLimiterConfig `json:"rate_limiter_config,omitempty"`
}

// rateLimiterConfig / tokenBucket 对应 cloud-hypervisor I/O 限速配置。
// 使用令牌桶算法：size 为每个 refill_time 窗口内的令牌数（字节数）。
// refill_time 建议 ≥100ms（内置 cool_down_time），否则实际速率会低于预期。
type rateLimiterConfig struct {
	Bandwidth *tokenBucket `json:"bandwidth,omitempty"`
	Ops       *tokenBucket `json:"ops,omitempty"`
}

type tokenBucket struct {
	Size         int64 `json:"size"`
	OneTimeBurst int64 `json:"one_time_burst,omitempty"`
	RefillTime   int64 `json:"refill_time"` // ms
}

// vmRemoveDevice 用于 vm.remove-device 请求。
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

// --- 实例网络配置（存储在 instDir/<vmID>/network 文件） ---

type netConfig struct {
	Idx int
	Tap string
	MAC string
	IP  string
}

// --- 实例配置（存储在 instDir/<vmID>/config.json） ---

type instanceConfig struct {
	Distro        string `json:"distro"`
	CPU           int    `json:"cpu"`
	MemoryMB      int64  `json:"memory_mb"`
	DiskGB        int64  `json:"disk_gb"`
	BandwidthMbps int    `json:"bandwidth_mbps"` // 0 表示不限速
}

// New 创建 Manager。binaryPath 为 cloud-hypervisor 二进制路径，
// 为空时自动在常见位置查找。
func New(binaryPath string) (*Manager, error) {
	if binaryPath == "" || strings.HasPrefix(binaryPath, "unix://") {
		binaryPath = findBinary()
	}
	if _, err := os.Stat(binaryPath); err != nil {
		return nil, fmt.Errorf("cloud-hypervisor binary not found at %q: %w", binaryPath, err)
	}
	instDir := filepath.Join(imgDir, "instances")
	for _, d := range []string{instDir, runDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	return &Manager{binary: binaryPath, instDir: instDir}, nil
}

func findBinary() string {
	for _, c := range []string{
		"./scripts/cloud-hypervisor-static",
		"./cloud-hypervisor-static",
		"/usr/local/bin/cloud-hypervisor",
		"/usr/bin/cloud-hypervisor",
	} {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return "./cloud-hypervisor-static"
}

// ─── 网络辅助 ─────────────────────────────────────────────────────────────────

func (m *Manager) allocNetwork(vmID string) (*netConfig, error) {
	netFile := filepath.Join(m.instDir, vmID, "network")
	if data, err := os.ReadFile(netFile); err == nil {
		return parseNetFile(string(data))
	}

	// 扫描已有实例，找下一个可用的 IDX (2-254)
	idx := 2
	for ; idx < 255; idx++ {
		used := false
		entries, _ := os.ReadDir(m.instDir)
		for _, e := range entries {
			data, err := os.ReadFile(filepath.Join(m.instDir, e.Name(), "network"))
			if err != nil {
				continue
			}
			cfg, err := parseNetFile(string(data))
			if err != nil {
				continue
			}
			if cfg.Idx == idx {
				used = true
				break
			}
		}
		if !used {
			break
		}
	}
	if idx >= 255 {
		return nil, fmt.Errorf("no free IPs in %s.0/24", netPrefix)
	}

	cfg := &netConfig{
		Idx: idx,
		Tap: fmt.Sprintf("%s%d", tapBase, idx),
		MAC: fmt.Sprintf("52:54:00:00:00:%02x", idx),
		IP:  fmt.Sprintf("%s.%d", netPrefix, idx),
	}
	content := fmt.Sprintf("IDX=%d\nTAP=%s\nMAC=%s\nIP=%s\n", cfg.Idx, cfg.Tap, cfg.MAC, cfg.IP)
	_ = os.WriteFile(netFile, []byte(content), 0644)
	return cfg, nil
}

func parseNetFile(data string) (*netConfig, error) {
	cfg := &netConfig{}
	for _, line := range strings.Split(strings.TrimSpace(data), "\n") {
		kv := strings.SplitN(line, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "IDX":
			cfg.Idx, _ = strconv.Atoi(kv[1])
		case "TAP":
			cfg.Tap = kv[1]
		case "MAC":
			cfg.MAC = kv[1]
		case "IP":
			cfg.IP = kv[1]
		}
	}
	if cfg.Idx == 0 {
		return nil, fmt.Errorf("invalid network config")
	}
	return cfg, nil
}

func (m *Manager) setupNetwork(net *netConfig) error {
	// 创建网桥
	if err := runCmd("ip", "link", "show", bridge); err != nil {
		if err := runCmd("ip", "link", "add", bridge, "type", "bridge"); err != nil {
			return fmt.Errorf("create bridge: %w", err)
		}
		if err := runCmd("ip", "addr", "add", gwIP+"/24", "dev", bridge); err != nil {
			return fmt.Errorf("add bridge IP: %w", err)
		}
		if err := runCmd("ip", "link", "set", bridge, "up"); err != nil {
			return fmt.Errorf("bring up bridge: %w", err)
		}
	}
	// 开启 IP 转发
	_ = os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644)
	// NAT masquerade
	if outIf := defaultRouteIface(); outIf != "" {
		cidr := netPrefix + ".0/24"
		// -C 检查规则是否存在，失败则插入
		if runCmd("iptables", "-t", "nat", "-C", "POSTROUTING", "-s", cidr, "-o", outIf, "-j", "MASQUERADE") != nil {
			_ = runCmd("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", cidr, "-o", outIf, "-j", "MASQUERADE")
		}
	}
	// 创建 TAP 设备
	if runCmd("ip", "link", "show", net.Tap) != nil {
		if err := runCmd("ip", "tuntap", "add", net.Tap, "mode", "tap"); err != nil {
			return fmt.Errorf("create tap %s: %w", net.Tap, err)
		}
	}
	_ = runCmd("ip", "link", "set", net.Tap, "master", bridge)
	_ = runCmd("ip", "link", "set", net.Tap, "up")
	return nil
}

func defaultRouteIface() string {
	out, err := cmdOutput("ip", "route")
	if err != nil {
		return ""
	}
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
chpasswd:
  expire: false
  users:
    - name: root
      password: %s
      type: text
ssh_pwauth: true
disable_root: false
write_files:
  - path: /etc/resolv.conf
    content: |
      nameserver 1.1.1.1
    permissions: '0644'
runcmd:
  - sed -i 's/^#\?PermitRootLogin.*/PermitRootLogin yes/' /etc/ssh/sshd_config
  - systemctl restart sshd 2>/dev/null || service sshd restart 2>/dev/null || true
`, password)

	metaData := fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", vmID, vmID)

	var networkCfg string
	if distro == "alpine" {
		networkCfg = fmt.Sprintf(`version: 1
config:
  - type: physical
    name: eth0
    mac_address: "%s"
    subnets:
      - type: static
        address: %s/24
        gateway: %s
        dns_nameservers:
          - 1.1.1.1
`, net.MAC, net.IP, gwIP)
	} else {
		networkCfg = fmt.Sprintf(`version: 2
ethernets:
  eth0:
    match:
      macaddress: "%s"
    addresses: [%s/24]
    gateway4: %s
    nameservers:
      addresses: [1.1.1.1]
`, net.MAC, net.IP, gwIP)
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

// launchProcess 启动 cloud-hypervisor 进程（仅打开 API socket，不创建/启动 VM）。
func (m *Manager) launchProcess(vmID string) error {
	sockPath := m.sockPath(vmID)
	_ = os.Remove(sockPath)
	_ = os.Remove(m.serialSockPath(vmID))

	cmd := exec.Command(m.binary, "--api-socket", "path="+sockPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	// 将日志重定向到文件
	logFile, _ := os.OpenFile(filepath.Join(runDir, vmID+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if logFile != nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start cloud-hypervisor: %w", err)
	}
	_ = os.WriteFile(m.pidFile(vmID), []byte(strconv.Itoa(cmd.Process.Pid)), 0644)

	// 等待 API socket 就绪（最多 30s）
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

	// 磁盘配置：O_DIRECT 跳过宿主页缓存，多队列提升并发 I/O
	diskNumQueues := cpu
	if diskNumQueues > 4 {
		diskNumQueues = 4 // virtio-blk 多队列上限一般不超过 4 个
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

	// 预留 64 GB 热插拔地址空间，仅占虚拟地址，不消耗物理内存
	hotplugSize := int64(64 * 1024 * 1024 * 1024)

	return &vmCreateReq{
		Cpus: cpusConfig{
			BootVcpus: cpu,
			MaxVcpus:  64, // 热插拔上限，不影响实际使用核心数
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
		// 气球设备：guest OOM 时自动缩减释放内存给宿主机；
		// free_page_reporting 让 guest 主动上报空闲页，宿主机可将物理页归还给系统。
		Balloon: &balloonConfig{
			Size:              0,
			DeflateOnOom:      true,
			FreePageReporting: true,
		},
		// Serial 使用 Socket 模式，允许通过 Unix socket 进行交互式控制台访问。
		// 进程启动前已清理旧 socket，cloud-hypervisor 会在 VM boot 时创建新 socket。
		Serial: &consoleConfig{
			Mode:   "Socket",
			Socket: m.serialSockPath(vmID),
		},
		Console: &consoleConfig{Mode: "Off"},
	}, nil
}

// ─── VMManager 接口实现 ────────────────────────────────────────────────────────

func (m *Manager) CreateVM(ctx context.Context, req *agent.CmdCreateVM) error {
	vmID := req.VmId
	dir := filepath.Join(m.instDir, vmID)

	// 防止重复创建
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("instance %q already exists", vmID)
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	// 出错时清理
	var err error
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()

	distro := req.OsImage
	if distro == "" {
		distro = "debian"
	}

	icfg := &instanceConfig{
		Distro:        distro,
		CPU:           int(req.Cpu),
		MemoryMB:      req.RamMb,
		DiskGB:        req.DiskGb,
		BandwidthMbps: int(req.BandwidthMbps),
	}
	if err = m.saveInstanceConfig(vmID, icfg); err != nil {
		return err
	}

	// 复制 cmdline
	srcCmdline := filepath.Join(imgDir, distro, "cmdline")
	cmdlineData, readErr := os.ReadFile(srcCmdline)
	if readErr != nil {
		return fmt.Errorf("cmdline not found for distro %q: %w", distro, readErr)
	}
	if err = os.WriteFile(filepath.Join(dir, "cmdline"), cmdlineData, 0644); err != nil {
		return err
	}

	// 分配网络
	var netCfg *netConfig
	netCfg, err = m.allocNetwork(vmID)
	if err != nil {
		return err
	}

	// 准备磁盘
	if err = m.prepareDisks(vmID, distro, req.DiskGb); err != nil {
		return err
	}

	// 生成 cloud-init
	if err = m.genCloudInit(vmID, distro, req.RootPassword, netCfg); err != nil {
		return err
	}

	// 设置 TAP/桥接网络
	if err = m.setupNetwork(netCfg); err != nil {
		return err
	}

	// 启动 cloud-hypervisor 进程
	if err = m.launchProcess(vmID); err != nil {
		return err
	}

	// 构造并下发 VmConfig
	var vmCfg *vmCreateReq
	vmCfg, err = m.buildVmConfig(vmID, icfg, netCfg)
	if err != nil {
		return err
	}
	if err = m.apiPut(vmID, "vm.create", vmCfg); err != nil {
		return err
	}

	// 启动 VM
	err = m.apiPut(vmID, "vm.boot", nil)
	return err
}

func (m *Manager) StartVM(_ context.Context, vmID string) error {
	if !m.isRunning(vmID) {
		// 进程已死，重新启动进程并创建+启动 VM
		icfg, err := m.loadInstanceConfig(vmID)
		if err != nil {
			return fmt.Errorf("instance %q not found: %w", vmID, err)
		}
		netCfg, err := m.allocNetwork(vmID)
		if err != nil {
			return err
		}
		if err = m.setupNetwork(netCfg); err != nil {
			return err
		}
		if err = m.launchProcess(vmID); err != nil {
			return err
		}
		vmCfg, err := m.buildVmConfig(vmID, icfg, netCfg)
		if err != nil {
			return err
		}
		if err = m.apiPut(vmID, "vm.create", vmCfg); err != nil {
			return err
		}
		return m.apiPut(vmID, "vm.boot", nil)
	}

	// 进程存在，查询状态决定是否需要 boot
	var info vmInfoResp
	if err := m.apiGet(vmID, "vm.info", &info); err != nil {
		return err
	}
	switch info.State {
	case "Running":
		return nil // 已在运行
	case "Created":
		return m.apiPut(vmID, "vm.boot", nil)
	case "Paused":
		return m.apiPut(vmID, "vm.resume", nil)
	default:
		// Shutdown 状态：需要删除并重新创建 VM（cloud-hypervisor 不支持重启已 shutdown 的 VM）
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
		if err = m.apiPut(vmID, "vm.create", vmCfg); err != nil {
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
	// 优雅关机：发送 ACPI 电源键信号
	if err := m.apiPut(vmID, "vm.power-button", nil); err != nil {
		// 降级为直接 shutdown API
		_ = m.apiPut(vmID, "vm.shutdown", nil)
	}
	return nil
}

func (m *Manager) RestartVM(_ context.Context, vmID string) error {
	return m.apiPut(vmID, "vm.reboot", nil)
}

func (m *Manager) DeleteVM(_ context.Context, vmID string) error {
	// 停止进程
	if m.isRunning(vmID) {
		_ = m.apiPut(vmID, "vm.shutdown", nil)
		time.Sleep(3 * time.Second)
		_ = m.killProcess(vmID)
	}
	// 释放 TAP 设备
	netCfg, err := m.allocNetwork(vmID)
	if err == nil {
		_ = runCmd("ip", "link", "delete", netCfg.Tap)
	}
	// 清除运行时文件
	for _, f := range []string{
		m.pidFile(vmID),
		m.sockPath(vmID),
		m.serialSockPath(vmID),
		filepath.Join(runDir, vmID+".log"),
	} {
		_ = os.Remove(f)
	}
	// 删除实例数据目录
	return os.RemoveAll(filepath.Join(m.instDir, vmID))
}

func (m *Manager) UpdateVM(ctx context.Context, req *agent.CmdUpdateVM) error {
	if !m.isRunning(req.VmId) {
		return fmt.Errorf("VM %q is not running", req.VmId)
	}

	icfg, err := m.loadInstanceConfig(req.VmId)
	if err != nil {
		return err
	}

	resizeReq := &vmResizeReq{}
	needResize := false

	if req.Cpu > 0 {
		v := int(req.Cpu)
		resizeReq.DesiredVcpus = &v
		icfg.CPU = v
		needResize = true
	}
	if req.RamMb > 0 {
		b := req.RamMb * 1024 * 1024
		resizeReq.DesiredRam = &b
		icfg.MemoryMB = req.RamMb
		needResize = true
	}

	// 带宽变更：移除旧网卡，挂载带新限速配置的网卡（会短暂断网）
	if req.BandwidthMbps > 0 {
		icfg.BandwidthMbps = int(req.BandwidthMbps)
		nc, netErr := m.allocNetwork(req.VmId)
		if netErr == nil {
			_ = m.apiPut(req.VmId, "vm.remove-device", &vmRemoveDevice{ID: "net0"})
			newNet := netCfg{
				Tap:               nc.Tap,
				Mac:               nc.MAC,
				ID:                "net0",
				RateLimiterConfig: buildNetRateLimit(icfg.BandwidthMbps),
			}
			_ = m.apiPut(req.VmId, "vm.add-net", &newNet)
		}
	}

	_ = m.saveInstanceConfig(req.VmId, icfg)

	if !needResize {
		return nil
	}
	return m.apiPut(req.VmId, "vm.resize", resizeReq)
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

// ResetPassword 通过离线挂载磁盘镜像修改 root 密码。
// VM 必须处于停止状态（或将被临时停止）。
func (m *Manager) ResetPassword(ctx context.Context, vmID, password string) error {
	wasRunning := m.isRunning(vmID)
	if wasRunning {
		if err := m.StopVM(ctx, vmID, false); err != nil {
			return fmt.Errorf("stop VM before password reset: %w", err)
		}
		// 等待 VM 完全关机
		for i := 0; i < 20; i++ {
			var info vmInfoResp
			if m.apiGet(vmID, "vm.info", &info) == nil && info.State == "Shutdown" {
				break
			}
			time.Sleep(time.Second)
		}
	}

	disk := filepath.Join(m.instDir, vmID, "system.raw")
	err := offlineChpasswd(disk, password)

	if wasRunning {
		_ = m.StartVM(ctx, vmID)
	}
	return err
}

func (m *Manager) GetVMInfo(_ context.Context, vmID string) (*agent.VMSummary, error) {
	ip, _ := m.GetVMIP(nil, vmID)
	status := agent.VMStatus_VM_STATUS_STOPPED

	if m.isRunning(vmID) {
		var info vmInfoResp
		if err := m.apiGet(vmID, "vm.info", &info); err == nil {
			status = mapState(info.State)
		}
	}

	return &agent.VMSummary{
		VmId:   vmID,
		Status: status,
		Ip:     ip,
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
			continue // 目录不是有效实例
		}

		ip, _ := m.GetVMIP(nil, vmID)
		status := agent.VMStatus_VM_STATUS_STOPPED
		if m.isRunning(vmID) {
			var info vmInfoResp
			if m.apiGet(vmID, "vm.info", &info) == nil {
				status = mapState(info.State)
			}
		}
		result = append(result, &agent.VMSummary{
			VmId:   vmID,
			Status: status,
			Ip:     ip,
		})
	}
	return result, nil
}

func (m *Manager) GetVMIP(_ context.Context, vmID string) (string, error) {
	netCfg, err := m.allocNetwork(vmID)
	if err != nil {
		return "", err
	}
	return netCfg.IP, nil
}

func (m *Manager) GetVMMAC(_ context.Context, vmID string) (string, error) {
	netCfg, err := m.allocNetwork(vmID)
	if err != nil {
		return "", err
	}
	return netCfg.MAC, nil
}

func (m *Manager) GetSupportedImages(_ context.Context) ([]*agent.OSImageInfo, error) {
	var images []*agent.OSImageInfo
	for _, distro := range []string{"debian", "alpine"} {
		if _, err := os.Stat(filepath.Join(imgDir, distro, "current.raw")); err == nil {
			images = append(images, &agent.OSImageInfo{
				Id:   distro,
				Name: strings.Title(distro) + " (cloud-hypervisor)",
			})
		}
	}
	if len(images) == 0 {
		return nil, fmt.Errorf("no VM images found in %s; run update-vm-images.sh first", imgDir)
	}
	return images, nil
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

func mapState(state string) agent.VMStatus {
	switch state {
	case "Running":
		return agent.VMStatus_VM_STATUS_RUNNING
	case "Created", "Paused":
		return agent.VMStatus_VM_STATUS_CREATING
	case "Shutdown":
		return agent.VMStatus_VM_STATUS_STOPPED
	default:
		return agent.VMStatus_VM_STATUS_STOPPED
	}
}

// offlineChpasswd 使用 losetup + mount + chroot 离线修改 root 密码。
func offlineChpasswd(diskPath, password string) error {
	// 检查必要工具
	for _, tool := range []string{"losetup", "mount", "umount", "chroot"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("tool %q not found; cannot reset password offline", tool)
		}
	}

	// 挂载镜像为 loop 设备（自动分区扫描）
	out, err := cmdOutput("losetup", "--find", "--show", "--partscan", diskPath)
	if err != nil {
		return fmt.Errorf("losetup: %w", err)
	}
	loopDev := strings.TrimSpace(out)
	defer func() { _ = runCmd("losetup", "-d", loopDev) }()

	// 找到根分区（最大的 Linux 分区）
	rootPart, err := findRootPartition(loopDev)
	if err != nil {
		return fmt.Errorf("find root partition: %w", err)
	}

	mnt, err := os.MkdirTemp("", "ch-mnt-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(mnt)

	if err = runCmd("mount", rootPart, mnt); err != nil {
		return fmt.Errorf("mount %s: %w", rootPart, err)
	}
	defer func() { _ = runCmd("umount", mnt) }()

	// chroot + chpasswd
	chpasswdInput := fmt.Sprintf("root:%s\n", password)
	cmd := exec.Command("chroot", mnt, "bash", "-c", "echo '"+
		strings.ReplaceAll(chpasswdInput, "'", "'\\''")+"' | chpasswd")
	cmd.Stdin = strings.NewReader(chpasswdInput)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("chpasswd: %w: %s", err, out)
	}
	return nil
}

// findRootPartition 在 loop 设备上找到最大的可挂载分区。
// Alpine 镜像没有分区表，直接返回 loop 设备本身。
func findRootPartition(loopDev string) (string, error) {
	// 先尝试 loopDev 本身（Alpine）
	mnt, _ := os.MkdirTemp("", "ch-probe-")
	defer os.RemoveAll(mnt)
	if runCmd("mount", "-o", "ro", loopDev, mnt) == nil {
		_ = runCmd("umount", mnt)
		return loopDev, nil
	}

	// 枚举分区 p1, p2, p3, p4
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

// buildNetRateLimit 将 Mbps 转换为 cloud-hypervisor TokenBucket 网络限速配置。
// 使用 1000ms 刷新窗口（远大于内置 100ms cool_down_time），保证实际速率贴近目标值。
// one_time_burst 设为等于桶容量，允许短时突发（TCP 慢启动友好）。
// AttachTTY 连接到虚拟机的串口控制台（通过 Unix socket）并进行双向数据交互。
// 需要 VM 以 Socket 模式的 serial 配置启动（buildVmConfig 已配置）。
// cloud-hypervisor serial console 是原始字节流，不支持 PTY resize，resize 事件会被忽略。
func (m *Manager) AttachTTY(ctx context.Context, vmID string, stdin io.Reader, stdout io.Writer, resize <-chan manager.ResizeEvent) error {
	sockPath := m.serialSockPath(vmID)

	conn, err := net.DialTimeout("unix", sockPath, 10*time.Second)
	if err != nil {
		return fmt.Errorf("serial console unavailable (is VM running?): %w", err)
	}
	defer conn.Close()

	// ctx 取消时关闭连接，终止两个 Copy goroutine
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	// 丢弃 resize 事件（串口不支持窗口尺寸协商）
	go func() {
		for range resize {
		}
	}()

	errc := make(chan error, 2)
	go func() {
		_, copyErr := io.Copy(conn, stdin)
		errc <- copyErr
	}()
	go func() {
		_, copyErr := io.Copy(stdout, conn)
		errc <- copyErr
	}()

	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
}

func buildNetRateLimit(mbps int) *rateLimiterConfig {
	if mbps <= 0 {
		return nil
	}
	bytesPerSec := int64(mbps) * 1_000_000 / 8
	bucket := &tokenBucket{
		Size:         bytesPerSec, // 1s 窗口内允许的总字节数
		OneTimeBurst: bytesPerSec, // 首次突发额度（等于桶容量）
		RefillTime:   1000,        // ms，每 1s 补满一次
	}
	return &rateLimiterConfig{Bandwidth: bucket}
}
