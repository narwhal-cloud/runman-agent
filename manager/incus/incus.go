//go:build linux

package incus

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/netip"
	"runtime"
	"strings"
	"sync"
	"time"

	incus "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"

	"runman-agent/db"
	"runman-agent/manager"
	"runman-agent/proto/agent"
)

const (
	SocketPath  = "/var/lib/incus/unix.socket"
	IncusBridge = "incusbr0"
)

type Manager struct {
	client     incus.InstanceServer
	db         *db.DB
	ipv6Mode   string
	ipv6Subnet string
	ipv6Addr   string
	ipv6Iface  string
	// 每个容器分配的 IPv6 数量（非 /64 网段精细化分配，默认 1）
	ipv6Alloc int
	// 自定义 alpine 基础镜像别名（结合 podcctv/alpine-base 等定制镜像）
	alpineBase string
	// 私有镜像服务器（simplestreams）基址；非空时运行时构建镜像优先从此拉取
	imageMirror string
	// 欢迎页 / SSH 登录横幅
	bannerPreset string
	bannerText   string
	// 纯 IPv6 容器开关：开启后容器不分配 IPv4，仅配置静态 IPv6
	ipv6Only bool
	buildMu  sync.Mutex
	mu       sync.Mutex // 全局锁，确保同一时间只有一个 VM 操作
}

func New(database *db.DB, ipv6Mode, ipv6Subnet, ipv6Addr, ipv6Iface, bannerPreset, bannerText string, ipv6Alloc int, alpineBase string, ipv6Only bool, imageMirror string) (*Manager, error) {
	c, err := incus.ConnectIncusUnix(SocketPath, nil)
	if err != nil {
		return nil, fmt.Errorf("connect to incus: %w", err)
	}

	if ipv6Alloc <= 0 {
		ipv6Alloc = 1
	}
	if ipv6Alloc > 15 {
		return nil, fmt.Errorf("incus_ipv6_alloc must be between 1 and 15")
	}

	return &Manager{
		client:       c,
		db:           database,
		ipv6Mode:     ipv6Mode,
		ipv6Subnet:   ipv6Subnet,
		ipv6Addr:     ipv6Addr,
		ipv6Iface:    ipv6Iface,
		ipv6Alloc:    ipv6Alloc,
		alpineBase:   alpineBase,
		imageMirror:  imageMirror,
		bannerPreset: bannerPreset,
		bannerText:   bannerText,
		ipv6Only:     ipv6Only,
	}, nil
}

// --- 导出方法（带全局锁） ---

func (m *Manager) CreateVM(ctx context.Context, req *agent.CmdCreateVM) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.createVM(ctx, req)
}

func (m *Manager) DeleteVM(ctx context.Context, vmID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.deleteVM(ctx, vmID)
}

func (m *Manager) StartVM(ctx context.Context, vmID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startVM(ctx, vmID)
}

func (m *Manager) StopVM(ctx context.Context, vmID string, force bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopVM(ctx, vmID, force)
}

func (m *Manager) RestartVM(ctx context.Context, vmID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.restartVM(ctx, vmID)
}

func (m *Manager) ReinstallVM(ctx context.Context, req *agent.CmdReinstallVM) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	_ = m.deleteVM(ctx, req.VmId)
	return m.createVM(ctx, &agent.CmdCreateVM{
		VmId:          req.VmId,
		OsImage:       req.OsImage,
		RootPassword:  req.RootPassword,
		Cpu:           req.Cpu,
		RamMb:         req.RamMb,
		DiskGb:        req.DiskGb,
		BandwidthMbps: req.BandwidthMbps,
	})
}

// --- 内部方法（不带锁） ---

func (m *Manager) createVM(ctx context.Context, req *agent.CmdCreateVM) error {
	// 限制最低配置：CPU 1核，内存 128MB，磁盘 1GB
	if req.Cpu < 1 {
		req.Cpu = 1
	}
	if req.RamMb < 128 {
		req.RamMb = 128
	}
	if req.DiskGb < 1 {
		req.DiskGb = 1
	}

	// 0. 清理同名旧实例（如果存在）
	_ = m.deleteVM(ctx, req.VmId)

	// 1. 分配索引
	idx, err := m.db.NextIncusIdx()
	if err != nil {
		return err
	}

	ipv4, ipv6s := m.computeIPs(idx)
	hasV6 := len(ipv6s) > 0

	// 纯 IPv6 容器：必须已具备可用 IPv6 子网，且不分配 IPv4
	if m.ipv6Only {
		if !hasV6 {
			return fmt.Errorf("IPv6-only containers require an IPv6 subnet (set IPV6_MODE=subnet/snat); current ipv6_mode=%q", m.ipv6Mode)
		}
		ipv4 = "" // 不分配 IPv4
	}

	// 计算掩码以便在 cloud-init 中使用
	ipv6Mask := "64"
	if prefix, err := netip.ParsePrefix(m.ipv6Subnet); err == nil {
		ipv6Mask = fmt.Sprintf("%d", prefix.Bits())
	}
	gw6 := m.ipv6Gateway()

	// 转换镜像别名
	alias := req.OsImage
	if alias == "debian" {
		alias = "debian/13/cloud"
	} else if alias == "alpine" {
		// 支持使用定制 alpine 基础镜像（如 podcctv/alpine-base）替代内置镜像
		if m.alpineBase != "" {
			alias = m.alpineBase
		} else {
			alias = "alpine/3.23/cloud"
		}
	}

	// 自定义基础镜像（本地已导入的别名）走本地源，不走 simplestreams
	baseIsLocal := m.alpineBase != "" && req.OsImage == "alpine"

	arch := runtime.GOARCH
	if arch == "x86_64" || arch == "amd64" {
		arch = "amd64"
	} else if arch == "aarch64" || arch == "arm64" {
		arch = "arm64"
	}

	if !strings.Contains(alias, arch) {
		alias = fmt.Sprintf("%s/%s", alias, arch)
	}

	imageSource := api.InstanceSource{
		Type:     "image",
		Server:   "https://images.linuxcontainers.org",
		Protocol: "simplestreams",
		Alias:    alias,
	}

	readyAlias := alias + "/ready"
	_, _, err = m.client.GetImageAlias(readyAlias)
	if err != nil {
		m.buildMu.Lock()
		_, _, err = m.client.GetImageAlias(readyAlias)
		if err != nil {
			log.Printf("[Incus] Building ready image for %s...", alias)
			if buildErr := m.ensureReadyImage(ctx, alias, readyAlias, req.OsImage, baseIsLocal); buildErr != nil {
				m.buildMu.Unlock()
				return fmt.Errorf("auto-build image failed: %w", buildErr)
			}
		}
		m.buildMu.Unlock()
		imageSource.Server = ""
		imageSource.Protocol = ""
		imageSource.Alias = readyAlias
	} else {
		imageSource.Server = ""
		imageSource.Protocol = ""
		imageSource.Alias = readyAlias
	}

	// ready 镜像已预装软件包并配置好 sshd（CI 预构建或 ensureReadyImage 构建），
	// 实例级 cloud-init 只负责密码与网络，避免开机时的装包/服务操作
	userData := fmt.Sprintf(`#cloud-config
ssh_pwauth: true
disable_root: false
chpasswd:
  list: |
    root:%s
  expire: false
`, req.RootPassword)

	if req.OsImage == "alpine" {
		netConf := `      auto lo
      iface lo inet loopback

      auto eth0
`
		if ipv4 != "" {
			// 纯 IPv6 容器无 IPv4，DNS 改用 IPv6 公共解析器
			dns := "1.1.1.1"
			if m.ipv6Only {
				dns = "2606:4700:4700::1111"
			}
			netConf += fmt.Sprintf(`      iface eth0 inet static
        address %s
        netmask 255.255.240.0
        gateway 10.91.0.1
        dns-nameservers %s
`, ipv4, dns)
		} else {
			// 纯 IPv6 容器：仅保留 loopback 与 eth0 声明，不配置 IPv4
			netConf += `      # IPv6-only container: no IPv4 configured
`
		}

		if hasV6 {
			for i, a := range ipv6s {
				if i == 0 {
					netConf += fmt.Sprintf(`
      iface eth0 inet6 static
        address %s
        netmask %s
        gateway %s
`, a, ipv6Mask, gw6)
				} else {
					netConf += fmt.Sprintf(`
      iface eth0 inet6 static
        address %s
        netmask %s
`, a, ipv6Mask)
				}
			}
		}

		userData += fmt.Sprintf(`
write_files:
  - path: /etc/network/interfaces
    content: |
%s`, netConf)
	} else {
		networkConf := `      [Match]
      Name=eth0

      [Network]
`
		if ipv4 != "" {
			// 纯 IPv6 容器无 IPv4，DNS 改用 IPv6 公共解析器
			dns := "1.1.1.1"
			if m.ipv6Only {
				dns = "2606:4700:4700::1111"
			}
			networkConf += fmt.Sprintf(`      DNS=%s

      [Address]
      Address=%s/20

      [Route]
      Gateway=10.91.0.1
`, dns, ipv4)
		} else {
			// 纯 IPv6 容器：仅配置 IPv6 DNS，不配置 IPv4 地址与网关
			networkConf += `      DNS=2606:4700:4700::1111

`
		}

		if hasV6 {
			for i, a := range ipv6s {
				if i == 0 {
					networkConf += fmt.Sprintf(`
      [Address]
      Address=%s/%s

      [Route]
      Gateway=%s
`, a, ipv6Mask, gw6)
				} else {
					networkConf += fmt.Sprintf(`
      [Address]
      Address=%s/%s
`, a, ipv6Mask)
				}
			}
		}

		userData += fmt.Sprintf(`
write_files:
  - path: /etc/systemd/network/10-eth0.network
    content: |
%s  - path: /etc/cloud/cloud.cfg.d/99-disable-network-config.cfg
    content: |
      network: {config: disabled}
`, networkConf)
	}

	// 自定义欢迎页 / SSH 登录横幅：写入 motd（登录后）与 issue.net（登录前），
	// 并通过 sshd drop-in 启用 Banner；同时为所有镜像（含自定义 alpine-base）
	// 强制开启 root 密码登录，避免定制镜像缺少该配置导致 SSH 密码登录失败。
	banner := m.bannerContent()
	if banner != "" {
		bannerEsc := indentLines(banner, "      ")
		userData += fmt.Sprintf(`
  - path: /etc/motd
    content: |
%s
  - path: /etc/issue.net
    content: |
%s
  - path: /etc/ssh/sshd_config.d/99-runman.conf
    content: |
      PermitRootLogin yes
      PasswordAuthentication yes
      Banner /etc/issue.net
`, bannerEsc, bannerEsc)
	} else {
		// 即便不显示横幅，也确保容器支持 SSH 用户密码登录
		userData += `
  - path: /etc/ssh/sshd_config.d/99-runman.conf
    content: |
      PermitRootLogin yes
      PasswordAuthentication yes
`
	}

	userData += "runcmd:\n"
	if req.OsImage == "alpine" {
		userData += "  - rc-update add networking boot\n"
		userData += "  - ifdown eth0 || true\n"
		userData += "  - ifup eth0 || true\n"
	} else {
		userData += "  - rm -f /etc/systemd/network/10-cloud-init-*.network /run/systemd/network/10-cloud-init-*.network || true\n"
		userData += "  - systemctl restart systemd-networkd || true\n"
	}
	// 强制把密码登录策略同步进主 sshd_config（部分基础镜像未 Include drop-in 目录）
	if banner != "" {
		userData += `  - sed -i 's/^#\?Banner.*/Banner \/etc\/issue.net/' /etc/ssh/sshd_config
  - sed -i 's/^#\?PasswordAuthentication.*/PasswordAuthentication yes/' /etc/ssh/sshd_config
  - sed -i 's/^#\?PermitRootLogin.*/PermitRootLogin yes/' /etc/ssh/sshd_config
`
	} else {
		userData += `  - sed -i 's/^#\?PasswordAuthentication.*/PasswordAuthentication yes/' /etc/ssh/sshd_config
  - sed -i 's/^#\?PermitRootLogin.*/PermitRootLogin yes/' /etc/ssh/sshd_config
`
	}

	// 本地毫秒级守护：新 ready 镜像已烤入 sshd 自启，此处仅兼容旧版构建的 ready 镜像
	userData += `  - [ sh, -c, "rc-update add sshd default 2>/dev/null || systemctl enable ssh 2>/dev/null || true" ]
  - [ sh, -c, "rc-service sshd start 2>/dev/null || systemctl start ssh 2>/dev/null || true" ]
`

	config := map[string]string{
		"limits.cpu":           fmt.Sprintf("%d", req.Cpu),
		"limits.memory":        fmt.Sprintf("%dMB", req.RamMb),
		"cloud-init.user-data": userData,
	}

	nic := map[string]string{
		"type":    "nic",
		"network": IncusBridge,
	}
	// Incus 6.x 要求 ipv4.address=none 与 security.ipv4_filtering 同时使用。
	// 过滤也能阻止容器伪造 IPv4 源地址，确保纯 IPv6 模式不会旁路获得 IPv4。
	if m.ipv6Only || ipv4 == "" {
		nic["ipv4.address"] = "none"
		nic["security.ipv4_filtering"] = "true"
	} else {
		nic["ipv4.address"] = ipv4
		nic["security.ipv4_filtering"] = "true"
	}

	// 应用带宽限速
	if req.BandwidthMbps > 0 {
		bandwidth := fmt.Sprintf("%dMbit", req.BandwidthMbps)
		nic["limits.ingress"] = bandwidth
		nic["limits.egress"] = bandwidth
	}

	devices := map[string]map[string]string{
		"root": {
			"type": "disk",
			"path": "/",
			"pool": "default",
			"size": fmt.Sprintf("%dGiB", req.DiskGb),
		},
		"eth0": nic,
	}

	op, err := m.client.CreateInstance(api.InstancesPost{
		Name:   req.VmId,
		Type:   api.InstanceTypeContainer,
		Source: imageSource,
		InstancePut: api.InstancePut{
			Profiles: []string{}, // 禁用 default profile，避免配置冲突
			Config:   config,
			Devices:  devices,
		},
	})
	if err != nil {
		return fmt.Errorf("create instance: %w", err)
	}

	if err := op.Wait(); err != nil {
		return fmt.Errorf("wait for create: %w", err)
	}

	if err := m.startVM(ctx, req.VmId); err != nil {
		return err
	}
	// Not every custom Incus image ships cloud-init (including lightweight
	// Alpine images). Apply the same credentials and network configuration
	// through the Incus agent after boot so IPv6-only instances are usable
	// regardless of image cloud-init support.
	if err := m.configureRunningInstance(ctx, req.VmId, req.OsImage, req.RootPassword, ipv4, ipv6s, ipv6Mask, gw6); err != nil {
		_ = m.deleteVM(ctx, req.VmId)
		return fmt.Errorf("configure running instance: %w", err)
	}

	bizConf, _ := m.db.GetVMConfig(req.VmId)
	if bizConf == nil {
		bizConf = &db.VMConfig{VMID: req.VmId}
	}
	bizConf.CPU = int(req.Cpu)
	bizConf.MemoryMB = req.RamMb
	bizConf.DiskGB = req.DiskGb
	bizConf.Image = req.OsImage
	bizConf.BandwidthMbps = int(req.BandwidthMbps)
	bizConf.Status = "running"
	_ = m.db.SaveVMConfig(bizConf)

	iConf := &db.IncusVMConfig{
		VMID:      req.VmId,
		Idx:       idx,
		Container: req.VmId,
		Image:     req.OsImage,
		IPv4:      ipv4,
		IPv6:      firstOrEmpty(ipv6s),
		IPv6s:     strings.Join(ipv6s, ","),
		IPv6Only:  m.ipv6Only,
	}
	_ = m.db.SaveIncusConfig(iConf)

	return nil
}

func (m *Manager) deleteVM(ctx context.Context, vmID string) error {
	// 先删 DB 记录：DB 失败时中止，避免出现"实例已删但 DB 仍有记录"的不一致状态。
	// 若 DB 成功但后续实例删除失败，下次 Cleanup 会将其作为幽灵实例清理。
	if err := m.db.DeleteVMConfig(vmID); err != nil {
		log.Printf("[Incus][DeleteVM] error: failed to delete VMConfig for %s: %v", vmID, err)
		return fmt.Errorf("delete VMConfig: %w", err)
	}
	if err := m.db.DeleteIncusConfig(vmID); err != nil {
		log.Printf("[Incus][DeleteVM] warning: failed to delete IncusConfig for %s: %v", vmID, err)
	}

	// 强杀并删除实例
	stopOp, _ := m.client.UpdateInstanceState(vmID, api.InstanceStatePut{Action: "stop", Timeout: -1, Force: true}, "")
	if stopOp != nil {
		_ = stopOp.Wait()
	}

	op, err := m.client.DeleteInstance(vmID)
	if err != nil {
		return err
	}
	return op.Wait()
}

func (m *Manager) startVM(ctx context.Context, vmID string) error {
	op, err := m.client.UpdateInstanceState(vmID, api.InstanceStatePut{Action: "start", Timeout: -1}, "")
	if err != nil {
		return err
	}
	return op.Wait()
}

func (m *Manager) configureRunningInstance(ctx context.Context, vmID, distro, rootPassword, ipv4 string, ipv6s []string, ipv6Mask, gw6 string) error {
	if err := m.execInstanceWithRetry(ctx, vmID, []string{"/bin/sh", "-c", "mkdir -p /etc/network /etc/systemd/network /etc/ssh/sshd_config.d /usr/local/sbin /run/sshd"}); err != nil {
		return fmt.Errorf("prepare configuration directories: %w", err)
	}

	if err := m.putInstanceFile(vmID, "/run/runman-root-password", "root:"+rootPassword+"\n", 0o600); err != nil {
		return fmt.Errorf("stage root password: %w", err)
	}
	defer func() { _ = m.client.DeleteInstanceFile(vmID, "/run/runman-root-password") }()

	sshConfig := "PermitRootLogin yes\nPasswordAuthentication yes\n"
	banner := m.bannerContent()
	if banner != "" {
		sshConfig += "Banner /etc/issue.net\n"
		if err := m.putInstanceFile(vmID, "/etc/motd", banner+"\n", 0o644); err != nil {
			return fmt.Errorf("write motd: %w", err)
		}
		if err := m.putInstanceFile(vmID, "/etc/issue.net", banner+"\n", 0o644); err != nil {
			return fmt.Errorf("write SSH banner: %w", err)
		}
	}
	if err := m.putInstanceFile(vmID, "/etc/ssh/sshd_config.d/99-runman.conf", sshConfig, 0o644); err != nil {
		return fmt.Errorf("write SSH policy: %w", err)
	}

	var networkPath, networkConfig, activate string
	if distro == "alpine" {
		networkPath = "/etc/network/interfaces"
		networkConfig = "auto lo\niface lo inet loopback\n\nauto eth0\n"
		if ipv4 != "" {
			networkConfig += fmt.Sprintf("iface eth0 inet static\n  address %s\n  netmask 255.255.240.0\n  gateway 10.91.0.1\n", ipv4)
		} else {
			networkConfig += "# IPv6-only container: no IPv4 configured\n"
		}
		for i, addr := range ipv6s {
			networkConfig += fmt.Sprintf("\niface eth0 inet6 static\n  address %s\n  netmask %s\n", addr, ipv6Mask)
			if i == 0 {
				networkConfig += fmt.Sprintf("  gateway %s\n  dns-nameservers 2606:4700:4700::1111\n", gw6)
			}
		}
		activate = "if command -v rc-service >/dev/null 2>&1; then rc-update add networking boot >/dev/null 2>&1 || true; rc-update add sshd default >/dev/null 2>&1 || true; rc-service networking restart; ssh-keygen -A; rc-service sshd restart; else /usr/local/sbin/runman-network-init; ssh-keygen -A; grep -qF '::wait:/usr/local/sbin/runman-network-init' /etc/inittab || echo '::wait:/usr/local/sbin/runman-network-init' >> /etc/inittab; grep -qF '::respawn:/usr/sbin/sshd -D -e' /etc/inittab || echo '::respawn:/usr/sbin/sshd -D -e' >> /etc/inittab; pkill sshd >/dev/null 2>&1 || true; /usr/sbin/sshd; fi"
	} else {
		networkPath = "/etc/systemd/network/10-eth0.network"
		networkConfig = "[Match]\nName=eth0\n\n[Network]\nDNS=2606:4700:4700::1111\n"
		if ipv4 != "" {
			networkConfig += fmt.Sprintf("\n[Address]\nAddress=%s/20\n\n[Route]\nGateway=10.91.0.1\n", ipv4)
		}
		for i, addr := range ipv6s {
			networkConfig += fmt.Sprintf("\n[Address]\nAddress=%s/%s\n", addr, ipv6Mask)
			if i == 0 {
				networkConfig += fmt.Sprintf("\n[Route]\nGateway=%s\n", gw6)
			}
		}
		activate = "rm -f /etc/systemd/network/10-cloud-init-*.network /run/systemd/network/10-cloud-init-*.network; systemctl restart systemd-networkd; ssh-keygen -A; systemctl enable --now ssh"
	}
	if err := m.putInstanceFile(vmID, networkPath, networkConfig, 0o644); err != nil {
		return fmt.Errorf("write network configuration: %w", err)
	}
	if distro == "alpine" {
		fallbackNetworkInit := "#!/bin/sh\n" +
			"attempt=0\n" +
			"while [ \"$attempt\" -lt 30 ]; do\n" +
			"  if ip link show eth0 >/dev/null 2>&1 && ifup -f eth0; then exit 0; fi\n" +
			"  attempt=$((attempt + 1))\n" +
			"  sleep 1\n" +
			"done\n" +
			"exit 1\n"
		if err := m.putInstanceFile(vmID, "/usr/local/sbin/runman-network-init", fallbackNetworkInit, 0o755); err != nil {
			return fmt.Errorf("write Alpine network init fallback: %w", err)
		}
	}

	command := "chpasswd < /run/runman-root-password; rm -f /run/runman-root-password; " +
		"grep -q '^Include /etc/ssh/sshd_config.d/\\*.conf' /etc/ssh/sshd_config 2>/dev/null || echo 'Include /etc/ssh/sshd_config.d/*.conf' >> /etc/ssh/sshd_config; " + activate
	if err := m.execInstanceWithRetry(ctx, vmID, []string{"/bin/sh", "-c", command}); err != nil {
		return fmt.Errorf("activate instance configuration: %w", err)
	}
	return nil
}

func (m *Manager) putInstanceFile(vmID, path, content string, mode int) error {
	return m.client.CreateInstanceFile(vmID, path, incus.InstanceFileArgs{
		Content:   strings.NewReader(content),
		UID:       0,
		GID:       0,
		Mode:      mode,
		Type:      "file",
		WriteMode: "overwrite",
	})
}

func (m *Manager) execInstanceWithRetry(ctx context.Context, vmID string, command []string) error {
	var lastErr error
	for attempt := 0; attempt < 20; attempt++ {
		op, err := m.client.ExecInstance(vmID, api.InstanceExecPost{Command: command}, nil)
		if err == nil {
			if err = op.Wait(); err == nil {
				return nil
			}
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return lastErr
}

func (m *Manager) stopVM(ctx context.Context, vmID string, force bool) error {
	op, err := m.client.UpdateInstanceState(vmID, api.InstanceStatePut{Action: "stop", Timeout: -1, Force: force}, "")
	if err != nil {
		return err
	}
	return op.Wait()
}

func (m *Manager) restartVM(ctx context.Context, vmID string) error {
	op, err := m.client.UpdateInstanceState(vmID, api.InstanceStatePut{Action: "restart", Timeout: -1}, "")
	if err != nil {
		return err
	}
	return op.Wait()
}

func (m *Manager) ensureReadyImage(ctx context.Context, baseAlias, readyAlias, distro string, baseIsLocal bool) error {
	builderName := fmt.Sprintf("builder-%d", time.Now().Unix())

	pkgSSH := "openssh-server"
	pkgCron := "cron"
	if distro == "alpine" {
		pkgSSH = "openssh"
		pkgCron = "cronie"
	}

	aptConfig := ""
	if distro != "alpine" {
		aptConfig = "- [ sh, -c, \"echo 'Acquire::ForceIPv4 \\\"true\\\";' > /etc/apt/apt.conf.d/99force-ipv4\" ]\n  - apt-get update"
	}

	// packages 模块失败（网络未就绪/源故障）不会阻止 runcmd，因此 runcmd 里
	// 带重试兜底安装，且 build_done 必须在确认 sshd 存在后才落盘，
	// 避免把没有 sshd 的坏镜像发布成 ready 别名。
	installCmd := fmt.Sprintf("apt-get update && apt-get install -y bash wget curl %s sshpass sudo %s lsof iptables dos2unix", pkgSSH, pkgCron)
	if distro == "alpine" {
		installCmd = fmt.Sprintf("apk add --no-cache bash wget curl %s sshpass sudo %s lsof iptables dos2unix", pkgSSH, pkgCron)
	}

	builderUserData := fmt.Sprintf(`#cloud-config
ssh_pwauth: true
manage_resolv_conf: true
resolv_conf:
  nameservers: ['1.1.1.1', '8.8.8.8']
packages:
  - bash
  - wget
  - curl
  - %s
  - sshpass
  - sudo
  - %s
  - lsof
  - iptables
  - dos2unix
runcmd:
  %s
  - [ sh, -c, "for i in $(seq 1 30); do command -v sshd >/dev/null 2>&1 && break; %s; sleep 5; done" ]
  - mkdir -p /etc/ssh/sshd_config.d
  - [ sh, -c, "printf 'PermitRootLogin yes\\nPasswordAuthentication yes\\nListenAddress 0.0.0.0\\nListenAddress ::\\n' > /etc/ssh/sshd_config.d/99-runman.conf" ]
  - sed -i 's/^#\\?PermitRootLogin.*/PermitRootLogin yes/' /etc/ssh/sshd_config
  - sed -i 's/^#\\?PasswordAuthentication.*/PasswordAuthentication yes/' /etc/ssh/sshd_config
  - [ sh, -c, "systemctl enable ssh 2>/dev/null || rc-update add sshd default 2>/dev/null || true" ]
  - [ sh, -c, "systemctl enable %s 2>/dev/null || rc-update add %s default 2>/dev/null || true" ]
  - [ sh, -c, "command -v sshd >/dev/null 2>&1 && touch /root/build_done" ]
`, pkgSSH, pkgCron, aptConfig, installCmd, pkgCron, pkgCron)

	var builderSource api.InstanceSource
	if baseIsLocal {
		// 本地已导入的定制基础镜像（如 podcctv/alpine-base），不使用 simplestreams 远端
		builderSource = api.InstanceSource{
			Type:  "image",
			Alias: baseAlias,
		}
	} else if m.imageMirror != "" && distro == "alpine" {
		// 优先从私有 simplestreams 镜像服务器拉取 alpine 基础镜像
		builderSource = api.InstanceSource{
			Type:     "image",
			Server:   m.imageMirror,
			Protocol: "simplestreams",
			Alias:    baseAlias,
		}
	} else {
		builderSource = api.InstanceSource{
			Type:     "image",
			Server:   "https://images.linuxcontainers.org",
			Protocol: "simplestreams",
			Alias:    baseAlias,
		}
	}

	op, err := m.client.CreateInstance(api.InstancesPost{
		Name:   builderName,
		Type:   api.InstanceTypeContainer,
		Source: builderSource,
		InstancePut: api.InstancePut{
			Config: map[string]string{
				"cloud-init.user-data": builderUserData,
			},
		},
	})
	if err != nil {
		// 私有镜像服务器拉取失败（版本缺失/不可达/索引异常），回退到上游 linuxcontainers
		if m.imageMirror != "" && builderSource.Server == m.imageMirror {
			log.Printf("[Incus] mirror pull failed (%v), falling back to images.linuxcontainers.org", err)
			builderSource = api.InstanceSource{
				Type:     "image",
				Server:   "https://images.linuxcontainers.org",
				Protocol: "simplestreams",
				Alias:    baseAlias,
			}
			op, err = m.client.CreateInstance(api.InstancesPost{
				Name:   builderName,
				Type:   api.InstanceTypeContainer,
				Source: builderSource,
				InstancePut: api.InstancePut{
					Config: map[string]string{
						"cloud-init.user-data": builderUserData,
					},
				},
			})
		}
		if err != nil {
			return err
		}
	}
	_ = op.Wait()

	defer func() {
		_ = m.stopVM(ctx, builderName, true)
		op, _ := m.client.DeleteInstance(builderName)
		if op != nil {
			_ = op.Wait()
		}
	}()

	if err := m.startVM(ctx, builderName); err != nil {
		return err
	}

	log.Printf("[Incus] Waiting for builder %s to complete installation...", builderName)
	success := false
	for i := 0; i < 120; i++ {
		_, _, err := m.client.GetInstanceFile(builderName, "/root/build_done")
		if err == nil {
			success = true
			break
		}
		time.Sleep(5 * time.Second)
	}

	if !success {
		return fmt.Errorf("builder timed out or failed to install packages")
	}

	// 清理构建标记，避免其残留在发布的镜像里
	_ = m.client.DeleteInstanceFile(builderName, "/root/build_done")

	if err := m.stopVM(ctx, builderName, false); err != nil {
		return err
	}

	op, err = m.client.CreateImage(api.ImagesPost{
		Source: &api.ImagesPostSource{
			Type: "instance",
			Name: builderName,
		},
		Aliases: []api.ImageAlias{{Name: readyAlias}},
	}, nil)
	if err != nil {
		return err
	}
	return op.Wait()
}

// computeIPs 根据索引计算静态 IPv4 以及（IPv6 模式下）一组静态 IPv6 地址。
// 每个容器分配 m.ipv6Alloc 个 IPv6 地址，支持非 /64 网段的精细化分配
// （例如在一个 /64 内给某容器连续分配 10 个可用地址）。
func (m *Manager) computeIPs(idx int) (ipv4 string, ipv6s []string) {
	host := idx & 0xff
	subnet := (idx >> 8) & 0xf
	ipv4 = fmt.Sprintf("10.91.%d.%d", subnet, host)

	n := m.ipv6Alloc
	if n < 1 {
		n = 1
	}

	switch m.ipv6Mode {
	case "subnet":
		prefix, err := netip.ParsePrefix(m.ipv6Subnet)
		if err == nil && prefix.Addr().Is6() {
			prefix = prefix.Masked()
			// idx starts at 2. Reserve offset 0 and 1, then pack each VM's
			// allocation contiguously within the first /112.
			base := uint64(2 + (idx-2)*n)
			for k := 0; k < n; k++ {
				addr, ok := addIPv6Offset(prefix.Addr(), base+uint64(k))
				if !ok || !prefix.Contains(addr) {
					return ipv4, nil
				}
				ipv6s = append(ipv6s, addr.String())
			}
		}
	case "snat":
		base := idx * n
		for k := 0; k < n; k++ {
			ipv6s = append(ipv6s, fmt.Sprintf("fd91:cafe:cafe:10::%x", base+k))
		}
	}
	return
}

// ipv6Gateway 返回容器 IPv6 静态地址应使用的网关。
// subnet 模式下网关必须是 incusbr0 的桥地址（<前缀>::1），而非宿主机公网地址；
// snat 模式使用 ULA 网关；其余回退到宿主机地址。
func (m *Manager) ipv6Gateway() string {
	if m.ipv6Mode == "subnet" && m.ipv6Subnet != "" {
		if prefix, err := netip.ParsePrefix(m.ipv6Subnet); err == nil && prefix.Addr().Is6() {
			// The installer assigns the last address of the first /112 to
			// incusbr0. This avoids colliding with routed-prefix host ::1/128.
			if addr, ok := addIPv6Offset(prefix.Masked().Addr(), 0xffff); ok && prefix.Contains(addr) {
				return addr.String()
			}
		}
	}
	if m.ipv6Addr != "" {
		return m.ipv6Addr
	}
	return "fd91:cafe:cafe:10::1"
}

// addIPv6Offset adds a small allocation offset to an IPv6 address without
// relying on textual `::` placement. It therefore works for every valid CIDR
// spelling, including fully expanded and non-/64 prefixes.
func addIPv6Offset(addr netip.Addr, offset uint64) (netip.Addr, bool) {
	if !addr.Is6() {
		return netip.Addr{}, false
	}
	b := addr.As16()
	carry := offset
	for i := len(b) - 1; i >= 0 && carry > 0; i-- {
		sum := uint64(b[i]) + (carry & 0xff)
		b[i] = byte(sum)
		carry = (carry >> 8) + (sum >> 8)
	}
	if carry != 0 {
		return netip.Addr{}, false
	}
	return netip.AddrFrom16(b), true
}

// ── 自定义欢迎页 / SSH 登录横幅 ─────────────────────────────────────────────────

const (
	bannerDefault = `========================================
 NarwhalCloud NAT VPS — 欢迎使用
 本实例由 NarwhalCloud Agent 管理
 请勿进行未授权操作，所有行为均被记录
========================================`

	bannerMinimal = `NarwhalCloud NAT VPS — Authorized access only`

	// bannerProject 是面向客户的"预设项目"模板，部署方可按需替换文本。
	bannerProject = `========================================
  欢迎使用我们的 NAT VPS 服务
  - 控制面板：https://host.example.com
  - 文档中心：https://docs.example.com
  - 技术支持：support@example.com
  祝您使用愉快！
========================================`
)

// bannerContent 根据预设类型返回横幅文本。
// preset 取值：none / default / minimal / project / custom（custom 时使用 bannerText）。
func (m *Manager) bannerContent() string {
	switch m.bannerPreset {
	case "none", "":
		return ""
	case "custom":
		if strings.TrimSpace(m.bannerText) != "" {
			return m.bannerText
		}
		return bannerDefault
	case "minimal":
		return bannerMinimal
	case "project":
		return bannerProject
	default:
		return bannerDefault
	}
}

// indentLines 给文本的每一行加上统一前缀（用于嵌进 cloud-init write_files 的内容块）。
func indentLines(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}

// firstOrEmpty 返回切片首个元素，空切片返回空串。
func firstOrEmpty(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

func (m *Manager) ResetPassword(_ context.Context, vmID, password string) error {
	_, err := m.client.ExecInstance(vmID, api.InstanceExecPost{
		Command: []string{"bash", "-c", fmt.Sprintf("echo 'root:%s' | chpasswd", password)},
	}, nil)
	return err
}

func (m *Manager) GetVMInfo(_ context.Context, vmID string) (*agent.VMSummary, error) {
	state, _, err := m.client.GetInstanceState(vmID)
	if err != nil {
		return nil, err
	}

	var ips []string
	for _, iface := range state.Network {
		for _, addr := range iface.Addresses {
			if addr.Scope == "global" {
				ips = append(ips, addr.Address)
			}
		}
	}

	return &agent.VMSummary{
		VmId:      vmID,
		Status:    mapStatus(state.Status),
		Ips:       ips,
		RamUsedMb: state.Memory.Usage / 1024 / 1024,
	}, nil
}

func (m *Manager) ListVMs(_ context.Context) ([]*agent.VMSummary, error) {
	instances, err := m.client.GetInstances(api.InstanceTypeAny)
	if err != nil {
		return nil, err
	}

	var result []*agent.VMSummary
	for _, inst := range instances {
		summary, _ := m.GetVMInfo(nil, inst.Name)
		if summary != nil {
			result = append(result, summary)
		}
	}
	return result, nil
}

func (m *Manager) GetVMLocalIP(_ context.Context, vmID string) (string, error) {
	conf, err := m.db.GetIncusConfig(vmID)
	if err != nil {
		return "", err
	}
	parts := strings.SplitN(conf.IPv4, "/", 2)
	return parts[0], nil
}

func (m *Manager) GetVMLocalIPv6(_ context.Context, vmID string) (string, error) {
	conf, err := m.db.GetIncusConfig(vmID)
	if err != nil {
		return "", err
	}
	parts := strings.SplitN(conf.IPv6, "/", 2)
	return parts[0], nil
}

func (m *Manager) GetSupportedImages(_ context.Context) ([]*agent.OSImageInfo, error) {
	return []*agent.OSImageInfo{
		{Id: "debian", Name: "Debian (Incus)"},
		{Id: "alpine", Name: "Alpine (Incus)"},
	}, nil
}

func (m *Manager) AttachTTY(ctx context.Context, vmID string, stdin io.Reader, stdout io.Writer, resize <-chan manager.ResizeEvent) error {
	post := api.InstanceExecPost{
		Command:     []string{"bash"},
		WaitForWS:   true,
		Interactive: true,
	}

	go func() {
		for rs := range resize {
			_ = rs
		}
	}()

	args := &incus.InstanceExecArgs{
		Stdin:    stdin,
		Stdout:   stdout,
		Stderr:   stdout,
		Control:  nil,
		DataDone: make(chan bool),
	}

	op, err := m.client.ExecInstance(vmID, post, args)
	if err != nil {
		return err
	}

	go func() {
		<-ctx.Done()
	}()

	err = op.Wait()
	<-args.DataDone
	return err
}

func (m *Manager) Cleanup(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	instances, err := m.client.GetInstances(api.InstanceTypeAny)
	if err != nil {
		return err
	}

	// 获取数据库中登记的所有虚拟机，DB 出错时必须中止：
	// 空列表会把所有实例误判为幽灵实例并删除。
	configs, err := m.db.ListVMConfigs()
	if err != nil {
		log.Printf("[Incus] Cleanup: failed to list VM configs, skipping to avoid false deletions: %v", err)
		return err
	}
	registered := make(map[string]bool, len(configs))
	for _, c := range configs {
		registered[c.VMID] = true
	}

	// 统计平台管理的 Incus 实例数（排除构建器）
	var managedCount int
	for _, inst := range instances {
		if !strings.HasPrefix(inst.Name, "builder-") {
			managedCount++
		}
	}
	// 安全检查：DB 为空但 Incus 有实例，放弃本次清理。
	if len(configs) == 0 && managedCount > 0 {
		log.Printf("[Incus] Cleanup: DB has 0 configs but %d instances exist, skipping to avoid false deletions", managedCount)
		return nil
	}

	for _, inst := range instances {
		// 跳过已登记的
		if registered[inst.Name] {
			continue
		}
		// 跳过镜像构建器
		if strings.HasPrefix(inst.Name, "builder-") {
			continue
		}

		// 判定为幽灵实例，直接删除
		log.Printf("[Incus] Found ghost instance %s, deleting...", inst.Name)
		_ = m.deleteVM(ctx, inst.Name)
	}
	return nil
}

func (m *Manager) GetVMNetStats(_ context.Context, vmID string) (*manager.VMNetStats, error) {
	state, _, err := m.client.GetInstanceState(vmID)
	if err != nil {
		return nil, err
	}
	var in, out int64
	for name, iface := range state.Network {
		if name == "lo" {
			continue
		}
		in += iface.Counters.BytesReceived
		out += iface.Counters.BytesSent
	}
	return &manager.VMNetStats{
		VMID:     vmID,
		InBytes:  in,
		OutBytes: out,
	}, nil
}

func mapStatus(s string) agent.VMStatus {
	switch strings.ToLower(s) {
	case "running":
		return agent.VMStatus_VM_STATUS_RUNNING
	case "stopped":
		return agent.VMStatus_VM_STATUS_STOPPED
	case "starting", "stopping":
		return agent.VMStatus_VM_STATUS_CREATING
	default:
		return agent.VMStatus_VM_STATUS_ERROR
	}
}
