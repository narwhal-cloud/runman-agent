package monitor

import (
	"context"
	"runman-agent/proto/agent"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
	psnet "github.com/shirou/gopsutil/v4/net"
)

type HostStats struct {
	*agent.Heartbeat
	RamTotalMb  int64 `json:"ram_total_mb"`
	DiskTotalGb int64 `json:"disk_total_gb"`
	NetInTotal  int64 `json:"net_in_total"`
	NetOutTotal int64 `json:"net_out_total"`
}

type HostMonitor struct {
	mu         sync.Mutex
	lastNetIn  int64
	lastNetOut int64
	lastTime   time.Time
}

func NewHostMonitor() *HostMonitor {
	return &HostMonitor{
		lastTime: time.Now(),
	}
}

func (h *HostMonitor) GetStats(ctx context.Context) (*HostStats, error) {
	targetNIC, _ := ctx.Value("monitor_nic").(string)
	targetDisk, _ := ctx.Value("monitor_disk").(string)

	v, _ := mem.VirtualMemory()
	c, _ := cpu.Percent(0, false)

	diskPath := "/"
	if targetDisk != "" {
		diskPath = targetDisk
	}
	d, _ := disk.Usage(diskPath)

	now := time.Now()
	h.mu.Lock()
	elapsed := now.Sub(h.lastTime).Seconds()
	if elapsed < 0.1 {
		elapsed = 1.0
	}

	netStats, _ := psnet.IOCounters(true)
	var currentIn, currentOut int64
	for _, ns := range netStats {
		if targetNIC != "" {
			if ns.Name == targetNIC {
				currentIn = int64(ns.BytesRecv)
				currentOut = int64(ns.BytesSent)
				break
			}
		} else {
			name := ns.Name
			if name == "lo" || strings.HasPrefix(name, "veth") || strings.HasPrefix(name, "br-") || strings.HasPrefix(name, "docker") {
				continue
			}
			currentIn += int64(ns.BytesRecv)
			currentOut += int64(ns.BytesSent)
		}
	}

	rateIn := int64(float64(currentIn-h.lastNetIn) / elapsed)
	rateOut := int64(float64(currentOut-h.lastNetOut) / elapsed)

	totalIn := currentIn
	totalOut := currentOut

	if h.lastNetIn == 0 {
		rateIn, rateOut = 0, 0
	}

	h.lastNetIn, h.lastNetOut, h.lastTime = currentIn, currentOut, now
	h.mu.Unlock()

	hb := &agent.Heartbeat{
		Timestamp:  now.Unix(),
		CpuPct:     0,
		RamUsedMb:  0,
		DiskUsedGb: 0,
		NetInBps:   rateIn,
		NetOutBps:  rateOut,
	}

	var ramTotal, diskTotal int64
	if v != nil {
		hb.RamUsedMb = int64(v.Used / 1024 / 1024)
		ramTotal = int64(v.Total / 1024 / 1024)
	}
	if len(c) > 0 {
		hb.CpuPct = float32(c[0])
	}
	if d != nil {
		hb.DiskUsedGb = int64(d.Used / 1024 / 1024 / 1024)
		diskTotal = int64(d.Total / 1024 / 1024 / 1024)
	}

	return &HostStats{
		Heartbeat:   hb,
		RamTotalMb:  ramTotal,
		DiskTotalGb: diskTotal,
		NetInTotal:  totalIn,
		NetOutTotal: totalOut,
	}, nil
}
