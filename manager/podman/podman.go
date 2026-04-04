//go:build containers_image_openpgp

package podman

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"runman-agent/manager/podman/cpualloc"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/containers/podman/v5/libpod/define"
	"github.com/containers/podman/v5/pkg/api/handlers"
	"github.com/containers/podman/v5/pkg/bindings"
	"github.com/containers/podman/v5/pkg/bindings/containers"
	"github.com/containers/podman/v5/pkg/bindings/images"
	containerTypes "github.com/containers/podman/v5/pkg/domain/entities/types"
	"github.com/containers/podman/v5/pkg/specgen"
	dockerContainer "github.com/docker/docker/api/types/container"
	"github.com/opencontainers/runtime-spec/specs-go"
	netTypes "go.podman.io/common/libnetwork/types"
	"go.podman.io/image/v5/manifest"

	"runman-agent/db"
	"runman-agent/manager"
	"runman-agent/proto/agent"
)

const (
	SocketPath  = "unix:///run/podman/podman.sock"
	NetworkName = "narwhal-net"
)

func ptr[T any](v T) *T { return &v }

type Manager struct {
	ctx   context.Context
	db    *db.DB
	alloc *cpualloc.Allocator
}

func New(database *db.DB) (*Manager, error) {
	alloc := cpualloc.New(runtime.NumCPU())
	if vmConfigs, err2 := database.ListPodmanConfigs(); err2 == nil {
		for _, c := range vmConfigs {
			alloc.Restore(c.Cpuset)
		}
	}
	// 用 Background context 建立长期连接，避免 ping 超时取消后续所有操作
	ctx, err := bindings.NewConnection(context.Background(), SocketPath)
	if err != nil {
		return nil, err
	}
	// 用独立超时 context 验证连通性
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err = bindings.GetClient(pingCtx); err != nil {
		return nil, err
	}

	return &Manager{ctx: ctx, db: database, alloc: alloc}, nil
}

func (m *Manager) timeoutCtx() context.Context {
	ctx, cancel := context.WithTimeout(m.ctx, time.Minute)
	_ = cancel // timer goroutine exits when timeout fires; acceptable for short-lived ops
	return ctx
}

func buildResourceConfig(cpu int64, ramMb int64, cpuset string) specgen.ContainerResourceConfig {
	res := &specs.LinuxResources{}
	if ramMb > 0 {
		res.Memory = &specs.LinuxMemory{
			Limit:            ptr(ramMb * 1024 * 1024),
			DisableOOMKiller: ptr(false),
		}
	}
	if cpu > 0 {
		res.CPU = &specs.LinuxCPU{
			Quota:  ptr(cpu * 100000),
			Period: ptr(uint64(100000)),
			Cpus:   cpuset,
		}
	}
	return specgen.ContainerResourceConfig{
		ResourceLimits: res,
		Rlimits: []specs.POSIXRlimit{
			{Type: "nofile", Soft: 65535, Hard: 65535},
			{Type: "nproc", Soft: 65535, Hard: 65535},
		},
	}
}

func lxcfsMounts() []specs.Mount {
	paths := []struct{ dst, src string }{
		{"/proc/cpuinfo", "/var/lib/lxcfs/proc/cpuinfo"},
		{"/proc/meminfo", "/var/lib/lxcfs/proc/meminfo"},
		{"/proc/diskstats", "/var/lib/lxcfs/proc/diskstats"},
		{"/proc/stat", "/var/lib/lxcfs/proc/stat"},
		{"/proc/swaps", "/var/lib/lxcfs/proc/swaps"},
		{"/proc/uptime", "/var/lib/lxcfs/proc/uptime"},
		{"/proc/loadavg", "/var/lib/lxcfs/proc/loadavg"},
		{"/sys/devices/system/cpu", "/var/lib/lxcfs/sys/devices/system/cpu"},
		{"/proc/slabinfo", "/var/lib/lxcfs/proc/slabinfo"},
	}
	mounts := make([]specs.Mount, 0, len(paths))
	for _, p := range paths {
		mounts = append(mounts, specs.Mount{
			Destination: p.dst,
			Type:        "bind",
			Source:      p.src,
			Options:     []string{"rw", "bind"},
		})
	}
	return mounts
}

func (m *Manager) CreateVM(_ context.Context, req *agent.CmdCreateVM) error {
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

	// 1. 拉取镜像
	if _, err := images.Pull(m.timeoutCtx(), req.OsImage, &images.PullOptions{
		Policy: ptr("newer"),
	}); err != nil {
		return err
	}

	// 2. 确定 IP 和 MAC
	var macAddr, cpuSet string
	var ipv4, ipv6 net.IP
	if conf, err := m.db.GetPodmanConfig(req.VmId); err == nil && conf.IPv4 != "" {
		// 复用已有配置
		ipv4 = net.ParseIP(conf.IPv4)
		if conf.IPv6 != "" {
			ipv6 = net.ParseIP(conf.IPv6)
		}
		macAddr = conf.MAC
		cpuSet = conf.Cpuset
		log.Printf("[CreateVM] %s: reuse stored config ipv4=%s ipv6=%s mac=%s cpuset=%s", req.VmId, conf.IPv4, conf.IPv6, conf.MAC, conf.Cpuset)
	} else {
		log.Printf("[CreateVM] %s: no stored config (err=%v), creating temp container to get IP", req.VmId, err)
		// 创建临时容器获取 IPAM 分配的地址
		tempID := "tmp-" + req.VmId
		netOpts := map[string]netTypes.PerNetworkOptions{
			NetworkName: {},
		}
		res, err := containers.CreateWithSpec(m.timeoutCtx(), &specgen.SpecGenerator{
			ContainerBasicConfig: specgen.ContainerBasicConfig{
				Name:     tempID,
				Hostname: tempID,
				Remove:   ptr(true),
			},
			ContainerStorageConfig: specgen.ContainerStorageConfig{
				Image: req.OsImage,
			},
			ContainerNetworkConfig: specgen.ContainerNetworkConfig{
				Networks: netOpts,
			},
			ContainerHealthCheckConfig: specgen.ContainerHealthCheckConfig{
				HealthConfig:         &manifest.Schema2HealthConfig{Test: []string{"NONE"}},
				HealthLogDestination: "/tmp",
			},
		}, nil)
		if err != nil {
			log.Printf("[CreateVM] %s: temp container create failed: %v", req.VmId, err)
		} else {
			log.Printf("[CreateVM] %s: temp container created id=%s", req.VmId, res.ID)
			// 启动并获取信息
			if startErr := containers.Start(m.timeoutCtx(), res.ID, nil); startErr != nil {
				log.Printf("[CreateVM] %s: temp container start failed: %v", req.VmId, startErr)
			} else {
				inspect, _ := containers.Inspect(m.timeoutCtx(), res.ID, nil)
				ipv4, ipv6, err = m.getIPsFromInspect(inspect)
				if err != nil {
					log.Printf("[CreateVM] %s: getIPsFromInspect failed: %v", req.VmId, err)
				} else {
					log.Printf("[CreateVM] %s: got ipv4=%s ipv6=%s from temp container", req.VmId, ipv4, ipv6)
					for _, ins := range inspect.NetworkSettings.Networks {
						if macAddr == "" {
							macAddr = ins.MacAddress
						}
					}
					log.Printf("[CreateVM] %s: got mac=%s from temp container", req.VmId, macAddr)
				}
			}
			// 删除临时容器
			_ = m.deleteByID(res.ID)
			log.Printf("[CreateVM] %s: temp container deleted", req.VmId)
		}
	}

	// 3. 分配 cpuset
	alloc := false
	if cpuSet == "" && req.Cpu > 0 {
		cpuSet, _ = m.alloc.Allocate(int(req.Cpu))
		alloc = true
	}

	// 如果最终拿到了地址，则注入到创建参数中
	netOpt := netTypes.PerNetworkOptions{}
	if ipv4 != nil {
		netOpt.StaticIPs = append(netOpt.StaticIPs, ipv4)
		log.Printf("[CreateVM] %s: will use static ipv4=%s", req.VmId, ipv4)
	} else {
		log.Printf("[CreateVM] %s: WARNING no static IPv4 will be set, IP may change on restart", req.VmId)
	}
	if ipv6 != nil {
		netOpt.StaticIPs = append(netOpt.StaticIPs, ipv6)
		log.Printf("[CreateVM] %s: will use static ipv6=%s", req.VmId, ipv6)
	}
	if macAddr != "" {
		if hwAddr, err := net.ParseMAC(macAddr); err == nil {
			netOpt.StaticMAC = netTypes.HardwareAddr(hwAddr)
		}
	}

	// 限速选项
	if req.BandwidthMbps > 0 {
		rate := int64(req.BandwidthMbps) * 1000000
		burst := rate / 10
		if burst < 1000000 {
			burst = 1000000
		}
		netOpt.Options = map[string]string{
			"bandwidth_rate":    fmt.Sprintf("%d", rate),
			"bandwidth_burst":   fmt.Sprintf("%d", burst),
			"bandwidth_latency": "50",
		}
	}

	netOpts := map[string]netTypes.PerNetworkOptions{
		NetworkName: netOpt,
	}

	res, err := containers.CreateWithSpec(m.timeoutCtx(), &specgen.SpecGenerator{
		ContainerBasicConfig: specgen.ContainerBasicConfig{
			Name:          req.VmId,
			Terminal:      ptr(true),
			Stdin:         ptr(true),
			RestartPolicy: "unless-stopped",
			Hostname:      req.VmId,
			Systemd:       "true",
		},
		ContainerStorageConfig: specgen.ContainerStorageConfig{
			Image: req.OsImage,
			StorageOpts: map[string]string{
				"size": fmt.Sprintf("%dG", req.DiskGb),
			},
			Mounts: lxcfsMounts(),
		},
		ContainerNetworkConfig: specgen.ContainerNetworkConfig{
			Networks:   netOpts,
			DNSServers: []net.IP{net.ParseIP("1.1.1.1"), net.ParseIP("2606:4700:4700::1111")},
		},
		ContainerResourceConfig: buildResourceConfig(int64(req.Cpu), req.RamMb, cpuSet),
		ContainerHealthCheckConfig: specgen.ContainerHealthCheckConfig{
			HealthConfig: &manifest.Schema2HealthConfig{
				Test:    []string{"NONE"},
				Timeout: 30 * time.Second,
			},
			HealthLogDestination: "/tmp",
		},
	}, nil)

	if err != nil {
		if alloc {
			m.alloc.Release(cpuSet)
		}
		return err
	}
	defer func() {
		if err != nil {
			_ = m.deleteByID(res.ID)
		}
	}()

	if err = containers.Start(m.timeoutCtx(), res.ID, nil); err != nil {
		return err
	}

	// 在正式容器启动后，如果 ipAdds 或 macAddr 为空（例如临时容器获取失败），从正式容器重新获取并更新到数据库
	if ipv4 == nil || macAddr == "" {
		inspect, _ := containers.Inspect(m.timeoutCtx(), res.ID, nil)
		if ipv4 == nil {
			ipv4, ipv6, _ = m.getIPsFromInspect(inspect)
		}
		if macAddr == "" {
			for _, netw := range inspect.NetworkSettings.Networks {
				if netw.MacAddress != "" {
					macAddr = netw.MacAddress
					break
				}
			}
		}
	}

	// 成功后确保保存配置到数据库
	bizConf, err := m.db.GetVMConfig(req.VmId)
	if err != nil {
		bizConf = &db.VMConfig{VMID: req.VmId}
	}
	bizConf.CPU = int(req.Cpu)
	bizConf.MemoryMB = req.RamMb
	bizConf.DiskGB = req.DiskGb
	bizConf.BandwidthMbps = int(req.BandwidthMbps)
	bizConf.Image = req.OsImage
	_ = m.db.SaveVMConfig(bizConf)

	// Podman 驱动特定配置
	pConf, err := m.db.GetPodmanConfig(req.VmId)
	if err != nil {
		pConf = &db.PodmanVMConfig{VMID: req.VmId}
	}
	pConf.Container = req.VmId
	pConf.IPv4 = ipv4.String()
	if ipv6 != nil {
		pConf.IPv6 = ipv6.String()
	}
	pConf.MAC = macAddr
	pConf.Cpuset = cpuSet
	_ = m.db.SavePodmanConfig(pConf)

	// 设置 root 密码
	err = m.execInContainer(res.ID, []string{"bash", "-c", fmt.Sprintf("echo 'root:%s' | chpasswd", req.RootPassword)})
	return err
}

func (m *Manager) execInContainer(id string, cmd []string) error {
	execID, err := containers.ExecCreate(m.timeoutCtx(), id, &handlers.ExecCreateConfig{
		ExecOptions: dockerContainer.ExecOptions{
			AttachStdout: true,
			AttachStderr: true,
			Cmd:          cmd,
		},
	})
	if err != nil {
		return err
	}
	return containers.ExecStart(m.timeoutCtx(), execID, nil)
}

func (m *Manager) deleteByID(id string) error {
	info, err := containers.Inspect(m.timeoutCtx(), id, nil)
	if err != nil {
		return err
	}

	// 删除前尝试从 DB 获取 cpuset 并释放
	if conf, err := m.db.GetPodmanConfig(info.Name); err == nil && conf.Cpuset != "" {
		m.alloc.Release(conf.Cpuset)
		_ = m.db.DeletePodmanConfig(info.Name)
	} else if conf, err := m.db.GetPodmanConfig(id); err == nil && conf.Cpuset != "" {
		m.alloc.Release(conf.Cpuset)
		_ = m.db.DeletePodmanConfig(id)
	}

	if info.State.Running {
		_ = containers.Stop(m.timeoutCtx(), id, nil)
	}
	_, err = containers.Remove(m.timeoutCtx(), id, &containers.RemoveOptions{Force: ptr(true)})
	return err
}

func (m *Manager) StartVM(_ context.Context, vmID string) error {
	return containers.Start(m.timeoutCtx(), vmID, nil)
}

func (m *Manager) StopVM(_ context.Context, vmID string, force bool) error {
	if force {
		return containers.Kill(m.timeoutCtx(), vmID, &containers.KillOptions{Signal: ptr("SIGKILL")})
	}
	return containers.Stop(m.timeoutCtx(), vmID, nil)
}

func (m *Manager) RestartVM(_ context.Context, vmID string) error {
	return containers.Restart(m.timeoutCtx(), vmID, nil)
}

func (m *Manager) DeleteVM(_ context.Context, vmID string) error {
	_ = m.db.DeleteVMConfig(vmID)
	return m.deleteByID(vmID)
}

func (m *Manager) UpdateVM(_ context.Context, vmID string, cpu int32, ramMB int64, _ int64, _ int32) error {
	resources := specs.LinuxResources{}
	changed := false
	if cpu > 0 {
		var cpuSet string
		if conf, err := m.db.GetPodmanConfig(vmID); err == nil {
			m.alloc.Release(conf.Cpuset)
			cpuSet, _ = m.alloc.Allocate(int(cpu))
			conf.Cpuset = cpuSet
			_ = m.db.SavePodmanConfig(conf)
		} else {
			cpuSet, _ = m.alloc.Allocate(int(cpu))
		}

		resources.CPU = &specs.LinuxCPU{
			Quota:  ptr(int64(100000) * int64(cpu)),
			Period: ptr(uint64(100000)),
			Cpus:   cpuSet,
		}
		changed = true
	}
	if ramMB > 0 {
		if ramMB < 128 {
			ramMB = 128
		}
		resources.Memory = &specs.LinuxMemory{
			Limit: ptr(ramMB * 1024 * 1024),
		}
		changed = true
	}
	if !changed {
		return nil
	}
	_, err := containers.Update(m.timeoutCtx(), &containerTypes.ContainerUpdateOptions{
		NameOrID:  vmID,
		Resources: &resources,
	})
	return err
}

func (m *Manager) ReinstallVM(ctx context.Context, req *agent.CmdReinstallVM) error {
	_ = m.StopVM(ctx, req.VmId, true)
	_ = m.DeleteVM(ctx, req.VmId)
	return m.CreateVM(ctx, &agent.CmdCreateVM{
		VmId:          req.VmId,
		OsImage:       req.OsImage,
		RootPassword:  req.RootPassword,
		Cpu:           req.Cpu,
		RamMb:         req.RamMb,
		DiskGb:        req.DiskGb,
		BandwidthMbps: req.BandwidthMbps,
	})
}

func (m *Manager) ResetPassword(_ context.Context, vmID, password string) error {
	return m.execInContainer(vmID, []string{"bash", "-c", fmt.Sprintf("echo 'root:%s' | chpasswd", password)})
}

func (m *Manager) GetVMLocalIP(_ context.Context, vmID string) (string, error) {
	inspect, err := containers.Inspect(m.timeoutCtx(), vmID, nil)
	if err != nil {
		return "", err
	}
	ipv4, _, err := m.getIPsFromInspect(inspect)
	if err != nil {
		return "", err
	}
	return ipv4.String(), nil
}

func (m *Manager) GetSupportedImages(_ context.Context) ([]*agent.OSImageInfo, error) {
	return []*agent.OSImageInfo{
		{Id: "docker.io/narwhalcloud/debian:podman", Name: "Debian (Podman)"},
		{Id: "docker.io/narwhalcloud/alpine:podman", Name: "Alpine (Podman)"},
	}, nil
}

func (m *Manager) GetVMInfo(ctx context.Context, vmID string) (*agent.VMSummary, error) {
	inspect, err := containers.Inspect(m.timeoutCtx(), vmID, nil)
	if err != nil {
		return nil, err
	}
	var ips []string
	ipv4, ipv6, _ := m.getIPsFromInspect(inspect)
	if ipv4 != nil {
		ips = append(ips, ipv4.String())
	}
	if ipv6 != nil {
		ips = append(ips, ipv6.String())
	}
	cpuPct, memUsed, netIn, netOut, _ := m.getUsage(ctx, vmID)
	return &agent.VMSummary{
		VmId:            vmID,
		Status:          mapStatus(inspect.State.Status),
		CpuPct:          cpuPct,
		RamUsedMb:       memUsed / 1024 / 1024,
		TrafficInBytes:  netIn,
		TrafficOutBytes: netOut,
		Ips:             ips,
	}, nil
}

func (m *Manager) getUsage(_ context.Context, vmID string) (cpuPct float32, memUsed, netIn, netOut int64, err error) {
	statsCtx, cancel := context.WithCancel(m.timeoutCtx())
	defer cancel()

	statsCh, err := containers.Stats(statsCtx, []string{vmID}, &containers.StatsOptions{
		All:      ptr(false),
		Stream:   ptr(true),
		Interval: ptr(1),
	})
	if err != nil {
		return
	}

	// 跳过第一个采样（基准值），取第二个计算速率
	i := 0
	for report := range statsCh {
		if i == 0 {
			i++
			continue
		}
		for _, s := range report.Stats {
			for _, n := range s.Network {
				netIn += int64(n.RxBytes)
				netOut += int64(n.TxBytes)
			}
			cpuPct = float32(s.CPU)
			memUsed = int64(s.MemUsage)
			cancel()
			go func() {
				for range statsCh {
				}
			}()
			return
		}
	}
	return
}

func (m *Manager) ListVMs(ctx context.Context) ([]*agent.VMSummary, error) {
	list, err := containers.List(m.timeoutCtx(), &containers.ListOptions{All: ptr(true)})
	if err != nil {
		return nil, err
	}

	summaries := make([]*agent.VMSummary, len(list))
	var wg sync.WaitGroup
	for i, c := range list {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			inspect, err2 := containers.Inspect(m.timeoutCtx(), id, nil)
			if err2 != nil {
				return
			}
			var ips []string
			ipv4, ipv6, _ := m.getIPsFromInspect(inspect)
			if ipv4 != nil {
				ips = append(ips, ipv4.String())
			}
			if ipv6 != nil {
				ips = append(ips, ipv6.String())
			}
			s := &agent.VMSummary{
				VmId:   inspect.Name,
				Status: mapStatus(inspect.State.Status),
				Ips:    ips,
			}
			if inspect.State.Running {
				cpuPct, memUsed, netIn, netOut, _ := m.getUsage(ctx, inspect.Name)
				s.CpuPct = cpuPct
				s.RamUsedMb = memUsed / 1024 / 1024
				s.TrafficInBytes = netIn
				s.TrafficOutBytes = netOut
			}
			summaries[i] = s
		}(i, c.ID)
	}
	wg.Wait()

	var result []*agent.VMSummary
	for _, s := range summaries {
		if s != nil {
			result = append(result, s)
		}
	}
	return result, nil
}

func (m *Manager) AttachTTY(ctx context.Context, vmID string, stdin io.Reader, stdout io.Writer, resize <-chan manager.ResizeEvent) error {
	execID, err := containers.ExecCreate(m.timeoutCtx(), vmID, &handlers.ExecCreateConfig{
		ExecOptions: dockerContainer.ExecOptions{
			AttachStdin:  true,
			AttachStdout: true,
			AttachStderr: true,
			Tty:          true,
			Cmd:          []string{"/bin/bash"},
		},
	})
	if err != nil {
		return err
	}

	// 合并 podman 连接 context 与请求取消信号：
	// 请求关闭时取消 attach，但保持底层 podman 连接可用。
	attachCtx, cancel := context.WithCancel(m.ctx)
	go func() {
		select {
		case <-ctx.Done():
			cancel()
		case <-attachCtx.Done():
		}
	}()
	defer cancel()

	// 处理终端尺寸变更
	go func() {
		for rs := range resize {
			h := int(rs.Rows)
			w := int(rs.Cols)
			_ = containers.ResizeExecTTY(m.ctx, execID,
				new(containers.ResizeExecTTYOptions).WithHeight(h).WithWidth(w))
		}
	}()

	return containers.ExecStartAndAttach(attachCtx, execID,
		new(containers.ExecStartAndAttachOptions).
			WithAttachInput(true).
			WithAttachOutput(true).
			WithAttachError(true).
			WithInputStream(*bufio.NewReader(stdin)).
			WithOutputStream(stdout).
			WithErrorStream(stdout),
	)
}

func (m *Manager) getIPsFromInspect(inspect *define.InspectContainerData) (ipv4, ipv6 net.IP, err error) {
	for _, ins := range inspect.NetworkSettings.Networks {
		ipv4 = net.ParseIP(ins.IPAddress)
		ipv6 = net.ParseIP(ins.GlobalIPv6Address)
	}
	if ipv4 == nil && ipv6 == nil {
		return nil, nil, fmt.Errorf("no ipv4 or ipv6 network found")
	}
	return ipv4, ipv6, nil
}

func mapStatus(s string) agent.VMStatus {
	switch strings.ToLower(s) {
	case "running":
		return agent.VMStatus_VM_STATUS_RUNNING
	case "exited", "stopped":
		return agent.VMStatus_VM_STATUS_STOPPED
	case "created":
		return agent.VMStatus_VM_STATUS_CREATING
	default:
		return agent.VMStatus_VM_STATUS_RUNNING
	}
}
