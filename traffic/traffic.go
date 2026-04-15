package traffic

import (
	"context"
	"fmt"
	"log"
	"runman-agent/config"
	"runman-agent/db"
	"runman-agent/manager"
	"sync"
	"time"
)

// VMNetStats 表示 VM 的网络流量统计
type VMNetStats struct {
	VMID      string
	InBytes   int64
	OutBytes  int64
	Timestamp time.Time
}

// Service 负责定期收集 VM 流量数据并同步到数据库
type Service struct {
	mgr    manager.VMManager
	db     *db.DB
	cfg    *config.Manager
	mu     sync.Mutex
	ticker *time.Ticker
	done   chan struct{}
}

// NewService 创建新的流量统计服务
func NewService(mgr manager.VMManager, database *db.DB, cfg *config.Manager) *Service {
	return &Service{
		mgr:  mgr,
		db:   database,
		cfg:  cfg,
		done: make(chan struct{}),
	}
}

// Start 启动后台定时同步任务
func (s *Service) Start(ctx context.Context, interval time.Duration) {
	s.mu.Lock()
	if s.ticker != nil {
		s.mu.Unlock()
		return
	}
	s.ticker = time.NewTicker(interval)
	s.mu.Unlock()

	go s.run(ctx)
	log.Printf("Traffic service started with interval %v", interval)
}

// Stop 停止服务
func (s *Service) Stop() {
	s.mu.Lock()
	if s.ticker != nil {
		s.ticker.Stop()
		s.ticker = nil
	}
	s.mu.Unlock()
	close(s.done)
}

func (s *Service) run(ctx context.Context) {
	// 启动时立即同步一次
	s.syncOnce(ctx)

	for {
		select {
		case <-s.done:
			return
		case <-s.ticker.C:
			s.syncOnce(ctx)
		}
	}
}

// syncOnce 执行一次流量同步
func (s *Service) syncOnce(ctx context.Context) {
	vms, err := s.mgr.ListVMs(ctx)
	if err != nil {
		log.Printf("failed to list VMs: %v", err)
		return
	}

	now := time.Now()

	// 从配置获取重置日期
	resetDay := s.cfg.Get().TrafficResetDay
	if resetDay <= 0 || resetDay > 28 {
		resetDay = 1
	}

	// 计算当前所处的计费周期键值 (例如 "2024-03-15")
	y, m, d := now.Date()
	if d < resetDay {
		m -= 1
		if m == 0 {
			m = 12
			y -= 1
		}
	}
	currentCycle := fmt.Sprintf("%04d-%02d-%02d", y, m, resetDay)

	for _, vm := range vms {
		// 从驱动获取 VM 的当前流量值（累计字节数）
		netStats, err := s.mgr.GetVMNetStats(ctx, vm.VmId)
		if err != nil {
			log.Printf("failed to get net stats for VM %s: %v", vm.VmId, err)
			continue
		}

		// 从数据库获取或创建 Traffic 记录
		traffic, err := s.db.GetTraffic(vm.VmId)
		if err != nil {
			// 新 VM，初始化记录
			traffic = &db.Traffic{
				VMID:      vm.VmId,
				RawIn:     netStats.InBytes,
				RawOut:    netStats.OutBytes,
				TotalIn:   0,
				TotalOut:  0,
				Month:     currentCycle,
				MonthIn:   0,
				MonthOut:  0,
				UpdatedAt: now,
			}
		} else {
			// 已有记录，计算增量
			deltaIn := netStats.InBytes - traffic.RawIn
			deltaOut := netStats.OutBytes - traffic.RawOut

			// 防止计数器重置（负值）
			if deltaIn < 0 {
				deltaIn = 0
			}
			if deltaOut < 0 {
				deltaOut = 0
			}

			// 累计到总数
			traffic.TotalIn += deltaIn
			traffic.TotalOut += deltaOut

			// 检查周期是否变化
			if traffic.Month != currentCycle {
				// 周期切换，重置月度数据
				traffic.Month = currentCycle
				traffic.MonthIn = 0
				traffic.MonthOut = 0
			}

			// 累计月度数据
			traffic.MonthIn += deltaIn
			traffic.MonthOut += deltaOut

			// 更新原始计数器值和时间戳
			traffic.RawIn = netStats.InBytes
			traffic.RawOut = netStats.OutBytes
			traffic.UpdatedAt = now
		}

		// 保存到数据库
		if err := s.db.SaveTraffic(traffic); err != nil {
			log.Printf("failed to save traffic for VM %s: %v", vm.VmId, err)
		}
	}
}

// GetVMTraffic 从数据库获取 VM 的累计流量（用于 API）
func (s *Service) GetVMTraffic(vmID string) (*db.Traffic, error) {
	return s.db.GetTraffic(vmID)
}
