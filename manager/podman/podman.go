//go:build containers_image_openpgp

package podman

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

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

	"runman-agent/manager"
	"runman-agent/proto/agent"
)

const networkName = "narwhal-net"

func ptr[T any](v T) *T { return &v }

type Manager struct {
	ctx context.Context
}

func New(socketPath string) (*Manager, error) {
	// 用 Background context 建立长期连接，避免 ping 超时取消后续所有操作
	ctx, err := bindings.NewConnection(context.Background(), socketPath)
	if err != nil {
		return nil, err
	}
	// 用独立超时 context 验证连通性
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err = bindings.GetClient(pingCtx); err != nil {
		return nil, err
	}

	return &Manager{ctx: ctx}, nil
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
			DisableOOMKiller: ptr(false), // 允许 OOM Killer 杀死失控的容器进程
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
			{Type: "RLIMIT_NOFILE", Soft: 65535, Hard: 65535}, // 提高文件描述符上限
			{Type: "RLIMIT_NPROC", Soft: 65535, Hard: 65535},  // 提高进程数上限
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

func (m *Manager) CreateVM(ctx context.Context, req *agent.CmdCreateVM) error {
	// 1. 拉取镜像
	_, err := images.Pull(m.timeoutCtx(), req.OsImage, &images.PullOptions{
		Policy: ptr("newer"),
	})
	if err != nil {
		return err
	}

	// 获取 context 中的 IP 和 MAC 地址（如果有的话）
	ipAddr := manager.IPAddressFrom(ctx)
	macAddr := manager.MacAddressFrom(ctx)

	// 2. 创建容器，如果没有指定就由 podman IPAM 自动分配 IP 和 MAC
	netOpt := netTypes.PerNetworkOptions{}

	// 如果有指定的 IP 就使用
	if ipAddr != "" {
		if parsedIP := net.ParseIP(ipAddr); parsedIP != nil {
			netOpt.StaticIPs = []net.IP{parsedIP}
		}
	}

	// 如果有指定的 MAC 就使用
	if macAddr != "" {
		if hwAddr, err := net.ParseMAC(macAddr); err == nil {
			netOpt.StaticMAC = netTypes.HardwareAddr(hwAddr)
		}
	}

	// 限速选项通过 PerNetworkOptions.Options 传递，Podman 会原样交给 netavark
	if req.BandwidthMbps > 0 {
		rate := int64(req.BandwidthMbps) * 1000000 // bits per second
		burst := rate / 10                         // 突发容量 = 100ms 流量
		if burst < 1000000 {
			burst = 1000000 // 最小 1 Mbit
		}
		netOpt.Options = map[string]string{
			"bandwidth_rate":    fmt.Sprintf("%d", rate),
			"bandwidth_burst":   fmt.Sprintf("%d", burst),
			"bandwidth_latency": "50",
		}
	}

	// netOpt 完整赋值后再放入 map
	netOpts := map[string]netTypes.PerNetworkOptions{
		networkName: netOpt,
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
		ContainerResourceConfig: buildResourceConfig(int64(req.Cpu), req.RamMb, manager.CpusetFrom(ctx)),
		ContainerHealthCheckConfig: specgen.ContainerHealthCheckConfig{
			HealthConfig: &manifest.Schema2HealthConfig{
				Test:    []string{"NONE"},
				Timeout: 30 * time.Second,
			},
			HealthLogDestination: "/tmp",
		},
	}, nil)

	if err != nil {
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
	return m.deleteByID(vmID)
}

func (m *Manager) UpdateVM(ctx context.Context, req *agent.CmdUpdateVM) error {
	resources := specs.LinuxResources{}
	changed := false
	if req.Cpu > 0 {
		resources.CPU = &specs.LinuxCPU{
			Quota:  ptr(int64(100000) * int64(req.Cpu)),
			Period: ptr(uint64(100000)),
			Cpus:   manager.CpusetFrom(ctx),
		}
		changed = true
	}
	if req.RamMb > 0 {
		resources.Memory = &specs.LinuxMemory{
			Limit: ptr(1024 * 1024 * req.RamMb),
		}
		changed = true
	}
	if !changed {
		return nil
	}
	_, err := containers.Update(m.timeoutCtx(), &containerTypes.ContainerUpdateOptions{
		NameOrID:  req.VmId,
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

func (m *Manager) GetVMIP(_ context.Context, vmID string) (string, error) {
	inspect, err := containers.Inspect(m.timeoutCtx(), vmID, nil)
	if err != nil {
		return "", err
	}
	for _, network := range inspect.NetworkSettings.Networks {
		if network.IPAddress != "" {
			return network.IPAddress, nil
		}
	}
	return inspect.NetworkSettings.IPAddress, nil
}

func (m *Manager) GetVMMAC(_ context.Context, vmID string) (string, error) {
	inspect, err := containers.Inspect(m.timeoutCtx(), vmID, nil)
	if err != nil {
		return "", err
	}
	for _, network := range inspect.NetworkSettings.Networks {
		if network.MacAddress != "" {
			return network.MacAddress, nil
		}
	}
	return inspect.NetworkSettings.MacAddress, nil
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

	ip := inspect.NetworkSettings.IPAddress
	for _, network := range inspect.NetworkSettings.Networks {
		if network.IPAddress != "" {
			ip = network.IPAddress
			break
		}
	}

	cpuPct, memUsed, netIn, netOut, _ := m.getUsage(ctx, vmID)
	return &agent.VMSummary{
		VmId:            vmID,
		Status:          mapStatus(inspect.State.Status),
		CpuPct:          cpuPct,
		RamUsedMb:       memUsed / 1024 / 1024,
		TrafficInBytes:  netIn,
		TrafficOutBytes: netOut,
		Ip:              ip,
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
			ip := inspect.NetworkSettings.IPAddress
			for _, network := range inspect.NetworkSettings.Networks {
				if network.IPAddress != "" {
					ip = network.IPAddress
					break
				}
			}
			s := &agent.VMSummary{
				VmId:   inspect.Name,
				Status: mapStatus(inspect.State.Status),
				Ip:     ip,
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
