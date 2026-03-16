//go:build containers_image_openpgp

package podman

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"strings"
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

	"runman-agent/proto/agent"
)

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
	_ = cancel // 调用方无需提前取消；超时本身会清理资源
	return ctx
}

func generateRandomMAC() (net.HardwareAddr, error) {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	buf[0] = (buf[0] | 0x02) & 0xfe
	mac, err := net.ParseMAC(fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x",
		buf[0], buf[1], buf[2], buf[3], buf[4], buf[5]))
	return mac, err
}

func buildResourceConfig(cpu int64, ramMb int64) specgen.ContainerResourceConfig {
	res := &specs.LinuxResources{
		BlockIO: &specs.LinuxBlockIO{Weight: ptr(uint16(1000))},
		Pids:    &specs.LinuxPids{Limit: 1024},
	}
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
		}
	}
	return specgen.ContainerResourceConfig{ResourceLimits: res}
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
		{"/sys/devices/system/cpu/online", "/var/lib/lxcfs/sys/devices/system/cpu/online"},
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
	// 1. 拉取镜像
	_, err := images.Pull(m.timeoutCtx(), req.OsImage, &images.PullOptions{
		Policy: ptr("newer"),
	})
	if err != nil {
		return err
	}

	// 2. 创建临时容器以获取网络 IP/MAC 分配
	tmpRes, err := containers.CreateWithSpec(m.timeoutCtx(), &specgen.SpecGenerator{
		ContainerBasicConfig: specgen.ContainerBasicConfig{
			Name:   req.VmId + "-tmp",
			Remove: ptr(true),
		},
		ContainerStorageConfig: specgen.ContainerStorageConfig{
			Image: req.OsImage,
		},
		ContainerNetworkConfig: specgen.ContainerNetworkConfig{
			Networks:       map[string]netTypes.PerNetworkOptions{"fuckme": {}},
			DNSServers:     []net.IP{net.ParseIP("1.1.1.1"), net.ParseIP("2606:4700:4700::1111")},
			NetworkOptions: map[string][]string{},
		},
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
	defer func() { _ = m.deleteByID(tmpRes.ID) }()

	if err = containers.Start(m.timeoutCtx(), tmpRes.ID, nil); err != nil {
		return err
	}
	inspect, err := containers.Inspect(m.timeoutCtx(), tmpRes.ID, nil)
	if err != nil {
		return err
	}

	// 从临时容器中收集 IP 和 MAC
	var ips []net.IP
	hw, _ := generateRandomMAC()
	for _, network := range inspect.NetworkSettings.Networks {
		if v4 := net.ParseIP(network.IPAddress); v4 != nil {
			ips = append(ips, v4)
		}
		if v6 := net.ParseIP(network.GlobalIPv6Address); v6 != nil {
			ips = append(ips, v6)
		}
		if mac, err2 := net.ParseMAC(network.MacAddress); err2 == nil {
			hw = mac
		}
		break
	}
	_ = containers.Stop(m.timeoutCtx(), tmpRes.ID, nil)

	// 3. 创建正式容器，使用静态 IP/MAC 保证网络地址稳定
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
			Networks: map[string]netTypes.PerNetworkOptions{
				"fuckme": {
					StaticIPs: ips,
					StaticMAC: netTypes.HardwareAddr(hw),
				},
			},
			DNSServers:     []net.IP{net.ParseIP("1.1.1.1"), net.ParseIP("2606:4700:4700::1111")},
			NetworkOptions: map[string][]string{},
		},
		ContainerResourceConfig: buildResourceConfig(int64(req.Cpu), req.RamMb),
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

func (m *Manager) UpdateVM(_ context.Context, req *agent.CmdUpdateVM) error {
	resources := specs.LinuxResources{}
	changed := false
	if req.Cpu > 0 {
		resources.CPU = &specs.LinuxCPU{
			Quota:  ptr(int64(100000) * int64(req.Cpu)),
			Period: ptr(uint64(100000)),
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
		VmId:         req.VmId,
		OsImage:      req.OsImage,
		RootPassword: req.RootPassword,
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

func (m *Manager) ListVMs(_ context.Context) ([]*agent.VMSummary, error) {
	list, err := containers.List(m.timeoutCtx(), &containers.ListOptions{All: ptr(true)})
	if err != nil {
		return nil, err
	}
	var summaries []*agent.VMSummary
	for _, c := range list {
		inspect, err2 := containers.Inspect(m.timeoutCtx(), c.ID, nil)
		if err2 != nil {
			continue
		}
		ip := inspect.NetworkSettings.IPAddress
		for _, network := range inspect.NetworkSettings.Networks {
			if network.IPAddress != "" {
				ip = network.IPAddress
				break
			}
		}
		summaries = append(summaries, &agent.VMSummary{
			VmId:   inspect.Name,
			Status: mapStatus(inspect.State.Status),
			Ip:     ip,
		})
	}
	return summaries, nil
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
