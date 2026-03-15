package monitor

import (
	"context"
	"runman-agent/proto/agent"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	psnet "github.com/shirou/gopsutil/v4/net"
)

type HostStats struct {
	*agent.Heartbeat
	NetInTotal  int64 `json:"net_in_total"`
	NetOutTotal int64 `json:"net_out_total"`
}

type HostMonitor struct {
	mu            sync.Mutex
	lastNetIn     int64
	lastNetOut    int64
	lastTime      time.Time
	bandwidthMbps int32
}

func NewHostMonitor() *HostMonitor {
	return &HostMonitor{
		lastTime: time.Now(),
	}
}

func (h *HostMonitor) SetBandwidth(mbps int32) {
	h.mu.Lock()
	h.bandwidthMbps = mbps
	h.mu.Unlock()
}

func (h *HostMonitor) GetStats(ctx context.Context) (*HostStats, error) {
	targetNIC, _ := ctx.Value("monitor_nic").(string)
	targetDisk, _ := ctx.Value("monitor_disk").(string)

	v, _ := mem.VirtualMemory()
	c, _ := cpu.Percent(0, false)
	cpuCount, _ := cpu.Counts(false)

	diskPath := "/"
	if targetDisk != "" {
		diskPath = targetDisk
	}
	d, _ := disk.Usage(diskPath)

	loadAvg, _ := load.Avg()
	uptime, _ := host.Uptime()

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

	if h.lastNetIn == 0 {
		rateIn, rateOut = 0, 0
	}

	bwMbps := h.bandwidthMbps
	h.lastNetIn, h.lastNetOut, h.lastTime = currentIn, currentOut, now
	h.mu.Unlock()

	hb := &agent.Heartbeat{
		Timestamp:     now.Unix(),
		NetInBps:      rateIn,
		NetOutBps:     rateOut,
		Uptime:        int64(uptime),
		Cpus:          int32(cpuCount),
		BandwidthMbps: bwMbps,
	}

	if v != nil {
		hb.RamUsedMb = int64(v.Used / 1024 / 1024)
		hb.RamTotalMb = int64(v.Total / 1024 / 1024)
	}
	if len(c) > 0 {
		hb.CpuPct = float32(c[0])
	}
	if d != nil {
		hb.DiskUsedGb = int64(d.Used / 1024 / 1024 / 1024)
		hb.DiskTotalGb = int64(d.Total / 1024 / 1024 / 1024)
	}
	if loadAvg != nil {
		hb.Load1 = float32(loadAvg.Load1)
		hb.Load5 = float32(loadAvg.Load5)
		hb.Load15 = float32(loadAvg.Load15)
	}

	return &HostStats{
		Heartbeat:   hb,
		NetInTotal:  currentIn,
		NetOutTotal: currentOut,
	}, nil
}
