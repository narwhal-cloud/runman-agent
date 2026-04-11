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

func (m *Manager) CreateVM(ctx context.Context, req *agent.CmdCreateVM) error {
	// 0. 清理同名旧实例（如果存在）
	_ = m.DeleteVM(ctx, req.VmId)

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

	// 转换镜像别名 (e.g. debian -> debian/13/cloud, alpine -> alpine/3.23/cloud)
	alias := req.OsImage
	if alias == "debian" {
		alias = "debian/13/cloud"
	} else if alias == "alpine" {
		alias = "alpine/3.23/cloud"
	}

	// 自动补全架构
	arch := runtime.GOARCH
	if arch == "x86_64" || arch == "amd64" {
		arch = "amd64"
	} else if arch == "aarch64" || arch == "arm64" {
		arch = "arm64"
	}

	if !strings.Contains(alias, arch) {
		alias = fmt.Sprintf("%s/%s", alias, arch)
	}

	// 3. 定义实例源：优先检查本地是否存在 ready 镜像
	imageSource := api.InstanceSource{
		Type:     "image",
		Server:   "https://images.linuxcontainers.org",
		Protocol: "simplestreams",
		Alias:    alias,
	}

	readyAlias := alias + "/ready"
	_, _, err = m.client.GetImageAlias(readyAlias)
	if err != nil {
		// 本地不存在 ready 镜像，开始自动构建（阻塞式）
		m.buildMu.Lock()
		// 再次检查防止并发冲突
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
		// 存在本地 ready 镜像，直接使用
		imageSource.Server = ""
		imageSource.Protocol = ""
		imageSource.Alias = readyAlias
	}

	// 确定包名
	pkgSSH := "openssh-server"
	pkgCron := "cron"
	if req.OsImage == "alpine" {
		pkgSSH = "openssh"
		pkgCron = "cronie"
	}

	// 强化 Cloud-init 配置：安装软件包、配置网络、开启 SSH
	// 在 /112 等小网段下，SLAAC 不起作用，必须通过 cloud-init 或 DHCPv6 强制静态配置

	// 1. 基础公共配置
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

	// 2. 写入网络配置（IPv6 仅在有有效地址时写入）
	// Alpine: /etc/network/interfaces (ifupdown 原生)
	// Debian: /etc/systemd/network/10-eth0.network (systemd-networkd 原生)
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
		// Debian: systemd-networkd 配置
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

	// 3. 合并所有的 runcmd
	userData += "runcmd:\n"

	// 网络重启逻辑
	if req.OsImage == "alpine" {
		userData += "  - rc-update add networking boot\n"
		userData += "  - ifdown eth0 || true\n"
		userData += "  - ifup eth0 || true\n"
	} else {
		// Debian: 移除 cloud-init 生成的 networkd 配置，使用我们自己的
		userData += "  - rm -f /etc/systemd/network/10-cloud-init-*.network /run/systemd/network/10-cloud-init-*.network || true\n"
		userData += "  - systemctl restart systemd-networkd || true\n"
	}

	// SSH 及其他公共逻辑
	userData += fmt.Sprintf(`  - [ sh, -c, "mkdir -p /etc/ssh/sshd_config.d && echo 'PermitRootLogin yes\nPasswordAuthentication yes\nListenAddress 0.0.0.0\nListenAddress ::' > /etc/ssh/sshd_config.d/99-runman.conf" ]
  - sed -i 's/^#\?PermitRootLogin.*/PermitRootLogin yes/' /etc/ssh/sshd_config
  - sed -i 's/^#\?PasswordAuthentication.*/PasswordAuthentication yes/' /etc/ssh/sshd_config
  - sed -i 's/^#\?ListenAddress 0.0.0.0/ListenAddress 0.0.0.0/' /etc/ssh/sshd_config
  - sed -i 's/^#\?ListenAddress ::/ListenAddress ::/' /etc/ssh/sshd_config
  - [ sh, -c, "systemctl enable ssh || systemctl enable sshd || rc-update add sshd default || true" ]
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
	// 不在 NIC 设备上设置 ipv6.address：
	// incusbr0 默认未开启 DHCPv6，设置任何 ipv6.address 值（包括 "none"）都会导致 Incus 报错。
	// IPv6 地址由 cloud-init 在容器内静态配置，无需 Incus 层面干预。

	devices := map[string]map[string]string{
		"root": {
			"type": "disk",
			"path": "/",
			"pool": "default",
			"size": fmt.Sprintf("%dGiB", req.DiskGb),
		},
		"eth0": nic,
	}

	// 4. 创建实例
	op, err := m.client.CreateInstance(api.InstancesPost{
		Name:   req.VmId,
		Type:   api.InstanceTypeContainer,
		Source: imageSource,
		InstancePut: api.InstancePut{
			Config:  config,
			Devices: devices,
		},
	})
	if err != nil {
		return fmt.Errorf("create instance: %w", err)
	}

	if err := op.Wait(); err != nil {
		return fmt.Errorf("wait for create: %w", err)
	}

	// 5. 启动实例
	if err := m.StartVM(ctx, req.VmId); err != nil {
		return err
	}

	// 6. 保存配置
	bizConf, _ := m.db.GetVMConfig(req.VmId)
	if bizConf == nil {
		bizConf = &db.VMConfig{VMID: req.VmId}
	}
	bizConf.CPU = int(req.Cpu)
	bizConf.MemoryMB = req.RamMb
	bizConf.DiskGB = req.DiskGb
	bizConf.Image = req.OsImage
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

func (m *Manager) ensureReadyImage(ctx context.Context, baseAlias, readyAlias, distro string) error {
	builderName := fmt.Sprintf("builder-%d", time.Now().Unix())

	// 确定包名
	pkgSSH := "openssh-server"
	pkgCron := "cron"
	if distro == "alpine" {
		pkgSSH = "openssh"
		pkgCron = "cronie"
	}

	// 使用 cloud-init 为 builder 安装基础包，这是最可靠的方式
	// 针对 Debian 13 (trixie) 强制使用 IPv4 解析
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

	// 1. 创建 builder 容器
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
		_ = m.StopVM(ctx, builderName, true)
		op, _ := m.client.DeleteInstance(builderName)
		if op != nil {
			_ = op.Wait()
		}
	}()

	// 2. 启动并等待任务完成
	if err := m.StartVM(ctx, builderName); err != nil {
		return err
	}

	log.Printf("[Incus] Waiting for builder %s to complete installation...", builderName)
	// 等待 cloud-init 完成（通过检查标记文件）
	success := false
	for i := 0; i < 120; i++ { // 最多等待 10 分钟
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

	// 3. 停止并发布
	if err := m.StopVM(ctx, builderName, false); err != nil {
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
	// IPv4: 10.91.x.y
	host := idx & 0xff
	subnet := (idx >> 8) & 0xf
	ipv4 = fmt.Sprintf("10.91.%d.%d", subnet, host)

	// IPv6
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

func (m *Manager) StartVM(_ context.Context, vmID string) error {
	op, err := m.client.UpdateInstanceState(vmID, api.InstanceStatePut{Action: "start", Timeout: -1}, "")
	if err != nil {
		return err
	}
	return op.Wait()
}

func (m *Manager) StopVM(_ context.Context, vmID string, force bool) error {
	op, err := m.client.UpdateInstanceState(vmID, api.InstanceStatePut{Action: "stop", Timeout: -1, Force: force}, "")
	if err != nil {
		return err
	}
	return op.Wait()
}

func (m *Manager) RestartVM(_ context.Context, vmID string) error {
	op, err := m.client.UpdateInstanceState(vmID, api.InstanceStatePut{Action: "restart", Timeout: -1}, "")
	if err != nil {
		return err
	}
	return op.Wait()
}

func (m *Manager) DeleteVM(_ context.Context, vmID string) error {
	_ = m.StopVM(nil, vmID, true)
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
	// 剥离掩码
	parts := strings.SplitN(conf.IPv4, "/", 2)
	return parts[0], nil
}

func (m *Manager) GetVMLocalIPv6(_ context.Context, vmID string) (string, error) {
	conf, err := m.db.GetIncusConfig(vmID)
	if err != nil {
		return "", err
	}
	// 剥离掩码
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

	// 处理 Resize 事件
	go func() {
		for rs := range resize {
			// Incus API 的 resize 需要通过 control 控制通道，此处由于库限制暂做占位
			_ = rs
		}
	}()

	args := &incus.InstanceExecArgs{
		Stdin:    stdin,
		Stdout:   stdout,
		Stderr:   stdout,
		Control:  nil, // 暂不实现动态 resize 控制流
		DataDone: make(chan bool),
	}

	op, err := m.client.ExecInstance(vmID, post, args)
	if err != nil {
		return err
	}

	// 监听 context 取消
	go func() {
		<-ctx.Done()
		// op.Cancel() 在 incus 库中可能不可用，但可以通过断开数据连接来触发
	}()

	err = op.Wait()
	<-args.DataDone
	return err
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
