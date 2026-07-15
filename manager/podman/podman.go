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
	entTypes "github.com/containers/podman/v5/pkg/domain/entities/types"
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
	mu    sync.Mutex // 全局锁，序列化所有容器生命周期操作，防止 IP/Cpuset 分配冲突
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

// createWithRetry 创建容器；若名字被残留容器占用（包括仅存在于 storage 层、
// libpod DB 不认识的 "external" 容器，重装/创建中途失败后可能残留），
// 强制清理同名容器后重试一次。
func (m *Manager) createWithRetry(spec *specgen.SpecGenerator) (entTypes.ContainerCreateResponse, error) {
	res, err := containers.CreateWithSpec(m.timeoutCtx(), spec, nil)
	if err != nil && strings.Contains(err.Error(), "already in use") {
		log.Printf("[Podman] create %s: name in use by stale container, force removing and retrying: %v", spec.Name, err)
		if _, rmErr := containers.Remove(m.timeoutCtx(), spec.Name, &containers.RemoveOptions{Force: ptr(true), Ignore: ptr(true)}); rmErr != nil {
			log.Printf("[Podman] create %s: stale container removal failed: %v", spec.Name, rmErr)
		}
		res, err = containers.CreateWithSpec(m.timeoutCtx(), spec, nil)
	}
	return res, err
}

func (m *Manager) CreateVM(ctx context.Context, req *agent.CmdCreateVM) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.createVM(ctx, req)
}

func (m *Manager) createVM(_ context.Context, req *agent.CmdCreateVM) error {
	// 限制最低配置：CPU 1核，内存 64MB，磁盘 1GB
	if req.Cpu < 1 {
		req.Cpu = 1
	}
	if req.RamMb < 64 {
		req.RamMb = 64
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
		res, err := m.createWithRetry(&specgen.SpecGenerator{
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
		})
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

	res, err := m.createWithRetry(&specgen.SpecGenerator{
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
	})

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
	name := id
	info, err := containers.Inspect(m.timeoutCtx(), id, nil)
	if err == nil {
		name = info.Name
		if info.State.Running {
			_ = containers.Stop(m.timeoutCtx(), id, nil)
		}
	} else {
		// Inspect 失败不代表容器不存在：可能是仅存在于 storage 层的 "external"
		// 容器（libpod DB 不认识），此时仍要继续走 Remove 清理 storage。
		log.Printf("[Podman] deleteByID %s: inspect failed (%v), removing anyway in case of storage-only container", id, err)
	}

	// 删除前尝试从 DB 获取 cpuset 并释放
	if conf, err := m.db.GetPodmanConfig(name); err == nil && conf.Cpuset != "" {
		m.alloc.Release(conf.Cpuset)
		_ = m.db.DeletePodmanConfig(name)
	} else if name != id {
		if conf, err := m.db.GetPodmanConfig(id); err == nil && conf.Cpuset != "" {
			m.alloc.Release(conf.Cpuset)
			_ = m.db.DeletePodmanConfig(id)
		}
	}

	// Ignore=true：容器（含 storage 层）都不存在时视为删除成功，保证幂等；
	// 名字对应 external 容器时，服务端会回退到按 storage 删除。
	_, err = containers.Remove(m.timeoutCtx(), name, &containers.RemoveOptions{Force: ptr(true), Ignore: ptr(true)})
	return err
}

func (m *Manager) StartVM(_ context.Context, vmID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startVM(vmID)
}

func (m *Manager) startVM(vmID string) error {
	return containers.Start(m.timeoutCtx(), vmID, nil)
}

func (m *Manager) StopVM(_ context.Context, vmID string, force bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopVM(vmID, force)
}

func (m *Manager) stopVM(vmID string, force bool) error {
	if force {
		return containers.Kill(m.timeoutCtx(), vmID, &containers.KillOptions{Signal: ptr("SIGKILL")})
	}
	return containers.Stop(m.timeoutCtx(), vmID, nil)
}

func (m *Manager) RestartVM(_ context.Context, vmID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return containers.Restart(m.timeoutCtx(), vmID, nil)
}

func (m *Manager) DeleteVM(ctx context.Context, vmID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.deleteVM(ctx, vmID)
}

func (m *Manager) deleteVM(ctx context.Context, vmID string) error {
	// 先删 DB 记录：DB 失败时中止，避免出现"容器已删但 DB 仍有记录"的不一致状态。
	if err := m.db.DeleteVMConfig(vmID); err != nil {
		log.Printf("[Podman][DeleteVM] error: failed to delete VMConfig for %s: %v", vmID, err)
		return fmt.Errorf("delete VMConfig: %w", err)
	}
	return m.deleteByID(vmID)
}

func (m *Manager) ReinstallVM(ctx context.Context, req *agent.CmdReinstallVM) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.stopVM(req.VmId, true); err != nil {
		log.Printf("[Podman][Reinstall] %s: stop failed (continuing): %v", req.VmId, err)
	}
	if err := m.deleteVM(ctx, req.VmId); err != nil {
		log.Printf("[Podman][Reinstall] %s: delete failed (continuing, create will retry on name conflict): %v", req.VmId, err)
	}
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

func (m *Manager) ResetPassword(_ context.Context, vmID, password string) error {
	// 密码重设通常涉及 exec，并不会导致并发资源冲突，但为了严谨也可以加锁或不加
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
			for name, n := range s.Network {
				if name == "lo" {
					continue
				}
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

// GetVMNetStats 获取 VM 的网络流量统计（用于流量统计服务）
func (m *Manager) GetVMNetStats(ctx context.Context, vmID string) (*manager.VMNetStats, error) {
	_, _, netIn, netOut, err := m.getUsage(ctx, vmID)
	if err != nil {
		return nil, err
	}
	return &manager.VMNetStats{
		VMID:     vmID,
		InBytes:  netIn,
		OutBytes: netOut,
	}, nil
}

func (m *Manager) Cleanup(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	list, err := containers.List(m.timeoutCtx(), &containers.ListOptions{All: ptr(true)})
	if err != nil {
		return err
	}

	// 获取数据库中登记的所有虚拟机，DB 出错时必须中止：
	// 空列表会把所有容器误判为幽灵实例并删除。
	configs, err := m.db.ListVMConfigs()
	if err != nil {
		log.Printf("[Podman] Cleanup: failed to list VM configs, skipping to avoid false deletions: %v", err)
		return err
	}
	registered := make(map[string]bool, len(configs))
	for _, c := range configs {
		registered[c.VMID] = true
	}

	// 安全检查：DB 为空但驱动有容器，可能是 DB 刚初始化或读取异常，放弃本次清理。
	if len(configs) == 0 && len(list) > 0 {
		log.Printf("[Podman] Cleanup: DB has 0 configs but %d containers exist, skipping to avoid false deletions", len(list))
		return nil
	}

	for _, c := range list {
		if len(c.Names) == 0 {
			continue
		}
		name := strings.TrimPrefix(c.Names[0], "/")

		// 跳过已登记的
		if registered[name] {
			continue
		}
		// 跳过临时容器
		if strings.HasPrefix(name, "tmp-") {
			continue
		}

		// 判定为幽灵实例，调用 deleteVM 确保 VMConfig 也被清理
		log.Printf("[Podman] Found ghost container %s, deleting...", name)
		_ = m.deleteVM(ctx, name)
	}
	return nil
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
