//go:build linux

package incus

import (
	"context"
	"fmt"
	"io"
	"log"
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
	buildMu    sync.Mutex
	mu         sync.Mutex // 全局锁，确保同一时间只有一个 VM 操作
}

func New(database *db.DB, ipv6Mode, ipv6Subnet, ipv6Addr, ipv6Iface string) (*Manager, error) {
	c, err := incus.ConnectIncusUnix(SocketPath, nil)
	if err != nil {
		return nil, fmt.Errorf("connect to incus: %w", err)
	}

	return &Manager{
		client:     c,
		db:         database,
		ipv6Mode:   ipv6Mode,
		ipv6Subnet: ipv6Subnet,
		ipv6Addr:   ipv6Addr,
		ipv6Iface:  ipv6Iface,
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
	// 0. 清理同名旧实例（如果存在）
	_ = m.deleteVM(ctx, req.VmId)

	// 1. 分配索引
	idx, err := m.db.NextIncusIdx()
	if err != nil {
		return err
	}

	ipv4, ipv6 := m.computeIPs(idx)

	// 计算掩码以便在 cloud-init 中使用
	ipv6Mask := "64"
	if m.ipv6Mode == "subnet" && m.ipv6Subnet != "" {
		parts := strings.SplitN(m.ipv6Subnet, "::/", 2)
		if len(parts) == 2 {
			ipv6Mask = parts[1]
		}
	}

	// 转换镜像别名
	alias := req.OsImage
	if alias == "debian" {
		alias = "debian/13/cloud"
	} else if alias == "alpine" {
		alias = "alpine/3.23/cloud"
	}

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
			if buildErr := m.ensureReadyImage(ctx, alias, readyAlias, req.OsImage); buildErr != nil {
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

	pkgSSH := "openssh-server"
	pkgCron := "cron"
	if req.OsImage == "alpine" {
		pkgSSH = "openssh"
		pkgCron = "cronie"
	}

	userData := fmt.Sprintf(`#cloud-config
ssh_pwauth: true
disable_root: false
chpasswd:
  list: |
    root:%s
  expire: false
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
`, req.RootPassword, pkgSSH, pkgCron)

	if req.OsImage == "alpine" {
		netConf := fmt.Sprintf(`      auto lo
      iface lo inet loopback

      auto eth0
      iface eth0 inet static
        address %s
        netmask 255.255.240.0
        gateway 10.91.0.1
        dns-nameservers 1.1.1.1
`, ipv4)

		if ipv6 != "" {
			gw6 := m.ipv6Addr
			if gw6 == "" {
				gw6 = "fd91:cafe:cafe:10::1"
			}
			netConf += fmt.Sprintf(`
      iface eth0 inet6 static
        address %s/%s
        gateway %s
`, ipv6, ipv6Mask, gw6)
		}

		userData += fmt.Sprintf(`
write_files:
  - path: /etc/network/interfaces
    content: |
%s`, netConf)
	} else {
		networkConf := fmt.Sprintf(`      [Match]
      Name=eth0

      [Network]
      DNS=1.1.1.1

      [Address]
      Address=%s/20

      [Route]
      Gateway=10.91.0.1
`, ipv4)

		if ipv6 != "" {
			gw6 := m.ipv6Addr
			if gw6 == "" {
				gw6 = "fd91:cafe:cafe:10::1"
			}
			networkConf += fmt.Sprintf(`
      [Address]
      Address=%s/%s

      [Route]
      Gateway=%s
`, ipv6, ipv6Mask, gw6)
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

	userData += `  - path: /etc/ssh/sshd_config
    content: |
      PermitRootLogin yes
      PasswordAuthentication yes
      ListenAddress 0.0.0.0
      ListenAddress ::
`
	userData += "runcmd:\n"
	if req.OsImage == "alpine" {
		userData += "  - rc-update add networking boot\n"
		userData += "  - ifdown eth0 || true\n"
		userData += "  - ifup eth0 || true\n"
	} else {
		userData += "  - rm -f /etc/systemd/network/10-cloud-init-*.network /run/systemd/network/10-cloud-init-*.network || true\n"
		userData += "  - systemctl restart systemd-networkd || true\n"
	}

	userData += fmt.Sprintf(`  - [ sh, -c, "systemctl enable ssh || systemctl enable sshd || rc-update add sshd default || true" ]
  - [ sh, -c, "systemctl restart ssh || systemctl restart sshd || rc-service sshd restart || true" ]
  - [ sh, -c, "systemctl enable %s || rc-update add %s default || true" ]
  - [ sh, -c, "systemctl restart %s || rc-service %s restart || true" ]
`, pkgCron, pkgCron, pkgCron, pkgCron)

	if req.OsImage != "alpine" {
		userData += "  - if [ -f /root/build_done ]; then rm /root/build_done; fi\n"
	}

	config := map[string]string{
		"limits.cpu":           fmt.Sprintf("%d", req.Cpu),
		"limits.memory":        fmt.Sprintf("%dMB", req.RamMb),
		"cloud-init.user-data": userData,
	}

	nic := map[string]string{
		"type":                    "nic",
		"network":                 IncusBridge,
		"ipv4.address":            ipv4,
		"security.ipv4_filtering": "true",
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
		IPv6:      ipv6,
	}
	_ = m.db.SaveIncusConfig(iConf)

	return nil
}

func (m *Manager) deleteVM(ctx context.Context, vmID string) error {
	// 强杀并删除
	stopOp, _ := m.client.UpdateInstanceState(vmID, api.InstanceStatePut{Action: "stop", Timeout: -1, Force: true}, "")
	if stopOp != nil {
		_ = stopOp.Wait()
	}

	op, err := m.client.DeleteInstance(vmID)
	if err != nil {
		return err
	}
	if err := op.Wait(); err != nil {
		return err
	}
	_ = m.db.DeleteVMConfig(vmID)
	_ = m.db.DeleteIncusConfig(vmID)
	return nil
}

func (m *Manager) startVM(ctx context.Context, vmID string) error {
	op, err := m.client.UpdateInstanceState(vmID, api.InstanceStatePut{Action: "start", Timeout: -1}, "")
	if err != nil {
		return err
	}
	return op.Wait()
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

func (m *Manager) ensureReadyImage(ctx context.Context, baseAlias, readyAlias, distro string) error {
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
  - mkdir -p /etc/ssh/sshd_config.d
  - [ sh, -c, "printf 'PermitRootLogin yes\\nPasswordAuthentication yes\\nListenAddress 0.0.0.0\\nListenAddress ::\\n' > /etc/ssh/sshd_config.d/99-runman.conf" ]
  - sed -i 's/^#\\?PermitRootLogin.*/PermitRootLogin yes/' /etc/ssh/sshd_config
  - sed -i 's/^#\\?PasswordAuthentication.*/PasswordAuthentication yes/' /etc/ssh/sshd_config
  - [ sh, -c, "touch /root/build_done" ]
`, pkgSSH, pkgCron, aptConfig)

	op, err := m.client.CreateInstance(api.InstancesPost{
		Name: builderName,
		Type: api.InstanceTypeContainer,
		Source: api.InstanceSource{
			Type:     "image",
			Server:   "https://images.linuxcontainers.org",
			Protocol: "simplestreams",
			Alias:    baseAlias,
		},
		InstancePut: api.InstancePut{
			Config: map[string]string{
				"cloud-init.user-data": builderUserData,
			},
		},
	})
	if err != nil {
		return err
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

func (m *Manager) computeIPs(idx int) (ipv4, ipv6 string) {
	host := idx & 0xff
	subnet := (idx >> 8) & 0xf
	ipv4 = fmt.Sprintf("10.91.%d.%d", subnet, host)

	switch m.ipv6Mode {
	case "subnet":
		if m.ipv6Subnet != "" {
			parts := strings.SplitN(m.ipv6Subnet, "::/", 2)
			if len(parts) == 2 {
				prefix := parts[0]
				ipv6 = fmt.Sprintf("%s::%x", prefix, idx)
			}
		}
	case "snat":
		ipv6 = fmt.Sprintf("fd91:cafe:cafe:10::%x", idx)
	}
	return
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

	// 获取数据库中登记的所有虚拟机
	configs, _ := m.db.ListVMConfigs()
	registered := make(map[string]bool, len(configs))
	for _, c := range configs {
		registered[c.VMID] = true
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
