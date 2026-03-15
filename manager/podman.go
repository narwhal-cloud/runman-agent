package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"runman-agent/proto/agent"
	"strings"
)

type PodmanManager struct {
	client     *http.Client
	socketPath string
}

func NewPodmanManager(socketPath string) (*PodmanManager, error) {
	path := strings.TrimPrefix(socketPath, "unix://")
	return &PodmanManager{
		socketPath: socketPath,
		client: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return net.Dial("unix", path)
				},
			},
		},
	}, nil
}

// 辅助方法：发送 POST 请求并确保关闭 Body，用于不需要解析返回值的场景
func (m *PodmanManager) postQuiet(url string, body io.Reader) error {
	resp, err := m.client.Post(url, "application/json", body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("podman api error (%d): %s", resp.StatusCode, string(b))
	}
	return nil
}

func (m *PodmanManager) CreateVM(ctx context.Context, req *agent.CmdCreateVM) error {
	// 1. Pull Image
	_ = m.postQuiet(fmt.Sprintf("http://d/v5.0.0/libpod/images/pull?reference=%s", req.OsImage), nil)

	// 2. Create
	body := map[string]interface{}{
		"name":  req.VmId,
		"image": req.OsImage,
		"resource_limits": map[string]interface{}{
			"cpu":    map[string]interface{}{"cpus": req.Cpu},
			"memory": map[string]interface{}{"limit": req.RamMb * 1024 * 1024},
		},
		"env":   map[string]string{"ROOT_PASSWORD": req.RootPassword},
		"netns": map[string]interface{}{"nsmode": "bridge"},
	}
	jsonBody, _ := json.Marshal(body)

	resp, err := m.client.Post("http://d/v5.0.0/libpod/containers/create", "application/json", strings.NewReader(string(jsonBody)))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	return m.StartVM(ctx, req.VmId)
}

func (m *PodmanManager) StartVM(_ context.Context, vmID string) error {
	return m.postQuiet(fmt.Sprintf("http://d/v5.0.0/libpod/containers/%s/start", vmID), nil)
}

func (m *PodmanManager) StopVM(_ context.Context, vmID string, force bool) error {
	url := fmt.Sprintf("http://d/v5.0.0/libpod/containers/%s/stop", vmID)
	if force {
		url = fmt.Sprintf("http://d/v5.0.0/libpod/containers/%s/kill", vmID)
	}
	return m.postQuiet(url, nil)
}

func (m *PodmanManager) RestartVM(_ context.Context, vmID string) error {
	return m.postQuiet(fmt.Sprintf("http://d/v5.0.0/libpod/containers/%s/restart", vmID), nil)
}

func (m *PodmanManager) DeleteVM(_ context.Context, vmID string) error {
	req, err := http.NewRequest("DELETE", fmt.Sprintf("http://d/v5.0.0/libpod/containers/%s?force=true", vmID), nil)
	if err != nil {
		return err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return nil
}

func (m *PodmanManager) UpdateVM(_ context.Context, req *agent.CmdUpdateVM) error {
	body := map[string]interface{}{}
	if req.Cpu > 0 {
		body["nano_cpus"] = int64(req.Cpu) * 1e9
	}
	if req.RamMb > 0 {
		body["memory"] = req.RamMb * 1024 * 1024
	}
	if len(body) == 0 {
		return nil
	}
	jsonBody, _ := json.Marshal(body)
	return m.postQuiet(fmt.Sprintf("http://d/v5.0.0/libpod/containers/%s/update", req.VmId), strings.NewReader(string(jsonBody)))
}

func (m *PodmanManager) ReinstallVM(ctx context.Context, req *agent.CmdReinstallVM) error {
	_ = m.StopVM(ctx, req.VmId, true)
	_ = m.DeleteVM(ctx, req.VmId)
	return m.CreateVM(ctx, &agent.CmdCreateVM{VmId: req.VmId, OsImage: req.OsImage, RootPassword: req.RootPassword})
}

func (m *PodmanManager) ResetPassword(_ context.Context, vmID, password string) error {
	body, _ := json.Marshal(map[string]interface{}{
		"Cmd": []string{"sh", "-c", fmt.Sprintf("echo 'root:%s' | chpasswd", password)},
	})
	resp, err := m.client.Post(fmt.Sprintf("http://d/v5.0.0/libpod/containers/%s/exec", vmID), "application/json", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	var res struct{ Id string }
	json.NewDecoder(resp.Body).Decode(&res)
	if res.Id == "" {
		return fmt.Errorf("failed to create exec session")
	}

	return m.postQuiet(fmt.Sprintf("http://d/v5.0.0/libpod/exec/%s/start", res.Id), strings.NewReader(`{"Detach":true}`))
}

func (m *PodmanManager) GetVMIP(ctx context.Context, vmID string) (string, error) {
	info, err := m.getContainerInfo(ctx, vmID)
	if err != nil {
		return "", err
	}
	return info.IP, nil
}

func (m *PodmanManager) GetSupportedImages(_ context.Context) ([]*agent.OSImageInfo, error) {
	return []*agent.OSImageInfo{
		{Id: "debian:stable-slim", Name: "Debian Stable (Container)"},
		{Id: "alpine:latest", Name: "Alpine Linux (Container)"},
	}, nil
}

func (m *PodmanManager) GetVMInfo(ctx context.Context, vmID string) (*agent.VMSummary, error) {
	info, err := m.getContainerInfo(ctx, vmID)
	if err != nil {
		return nil, err
	}
	cpuPct, memUsed, netIn, netOut, _ := m.getDetailedStats(ctx, vmID)
	return &agent.VMSummary{
		VmId:            vmID,
		Status:          m.mapStatus(info.State),
		CpuPct:          cpuPct,
		RamUsedMb:       memUsed / 1024 / 1024,
		TrafficInBytes:  netIn,
		TrafficOutBytes: netOut,
		Ip:              info.IP,
	}, nil
}

func (m *PodmanManager) ListVMs(ctx context.Context) ([]*agent.VMSummary, error) {
	resp, err := m.client.Get("http://d/v5.0.0/libpod/containers/json?all=true")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var list []struct{ Id string }
	json.NewDecoder(resp.Body).Decode(&list)
	var summaries []*agent.VMSummary
	for _, c := range list {
		if s, _ := m.GetVMInfo(ctx, c.Id); s != nil {
			summaries = append(summaries, s)
		}
	}
	return summaries, nil
}

// 内部辅助方法
func (m *PodmanManager) getContainerInfo(_ context.Context, id string) (*containerInfo, error) {
	url := fmt.Sprintf("http://d/v5.0.0/libpod/containers/%s/json", id)
	resp, err := m.client.Get(url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var i struct {
		State           struct{ Status string } `json:"State"`
		NetworkSettings struct {
			IPAddress string                                `json:"IPAddress"`
			Networks  map[string]struct{ IPAddress string } `json:"Networks"`
		} `json:"NetworkSettings"`
	}
	json.NewDecoder(resp.Body).Decode(&i)
	ip := i.NetworkSettings.IPAddress
	if ip == "" {
		for _, net := range i.NetworkSettings.Networks {
			if net.IPAddress != "" {
				ip = net.IPAddress
				break
			}
		}
	}
	return &containerInfo{ID: id, IP: ip, State: i.State.Status}, nil
}

func (m *PodmanManager) getDetailedStats(_ context.Context, id string) (float32, int64, int64, int64, error) {
	url := fmt.Sprintf("http://d/v5.0.0/libpod/containers/%s/stats?stream=false", id)
	resp, err := m.client.Get(url)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	var s struct {
		Stats []struct {
			CPU       float64 `json:"CPU"`
			MemUsage  uint64  `json:"MemUsage"`
			NetInput  uint64  `json:"NetInput"`
			NetOutput uint64  `json:"NetOutput"`
		} `json:"Stats"`
	}
	json.NewDecoder(resp.Body).Decode(&s)
	if len(s.Stats) > 0 {
		st := s.Stats[0]
		return float32(st.CPU), int64(st.MemUsage), int64(st.NetInput), int64(st.NetOutput), nil
	}
	return 0, 0, 0, 0, nil
}

func (m *PodmanManager) mapStatus(s string) agent.VMStatus {
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

type containerInfo struct{ ID, IP, State string }
