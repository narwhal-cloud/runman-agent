package manager

import (
	"context"
	"io"
	"log"
	"runman-agent/db"
	"runman-agent/proto/agent"
)

// VMService 是 VMManager 的服务层包装，位于 gRPC 处理层与底层虚拟化驱动之间，负责：
//  1. 将 platform 下发的 serverVmID 转换为底层虚拟化驱动实际使用的 localID
//  2. ListVMs 只返回本节点 DB 中已登记的 VM，过滤用户自行创建的容器/虚拟机
//  3. 业务编排：调用驱动执行生命周期管理，并补全 DB 配置
type VMService struct {
	mgr VMManager
	db  *db.DB

	// OnCreated 在 VM 成功创建并写入 DB 后被调用，可用于自动添加端口转发等。
	// bandwidthMbps 为 0 时表示不限速。
	OnCreated func(ctx context.Context, vmID string, bandwidthMbps int)
}

func NewVMService(mgr VMManager, database *db.DB) *VMService {
	return &VMService{mgr: mgr, db: database}
}

// --- 生命周期管理 ---

func (s *VMService) CreateVM(ctx context.Context, req *agent.CmdCreateVM) error {
	log.Printf("[CreateVM] start: vmID=%q, image=%q", req.VmId, req.OsImage)
	if err := s.mgr.CreateVM(ctx, req); err != nil {
		return err
	}

	// 驱动层已经负责将驱动特定配置（如 Podman 的 IP/MAC/Cpuset）存入数据库。
	// 这里补全业务层的通用配置。
	conf, _ := s.db.GetVMConfig(req.VmId)
	if conf == nil {
		conf = &db.VMConfig{VMID: req.VmId}
	}
	conf.BandwidthMbps = int(req.BandwidthMbps)
	conf.CPU = int(req.Cpu)
	conf.MemoryMB = req.RamMb
	conf.DiskGB = req.DiskGb
	conf.Image = req.OsImage
	conf.Status = "running"
	log.Printf("[CreateVM] saving VMConfig: vmID=%s, image=%s", conf.VMID, conf.Image)
	if err := s.db.SaveVMConfig(conf); err != nil {
		log.Printf("[CreateVM] SaveVMConfig error: %v", err)
		return err
	}

	if s.OnCreated != nil {
		s.OnCreated(ctx, req.VmId, int(req.BandwidthMbps))
	}
	return nil
}

func (s *VMService) StartVM(ctx context.Context, vmID string) error {
	log.Printf("[StartVM] vmID=%s", vmID)
	err := s.mgr.StartVM(ctx, vmID)
	if err != nil {
		log.Printf("[StartVM] error: vmID=%s, err=%v", vmID, err)
	} else {
		log.Printf("[StartVM] success: vmID=%s", vmID)
	}
	return err
}

func (s *VMService) StopVM(ctx context.Context, vmID string, force bool) error {
	forceStr := "graceful"
	if force {
		forceStr = "force"
	}
	log.Printf("[StopVM] vmID=%s, mode=%s", vmID, forceStr)
	err := s.mgr.StopVM(ctx, vmID, force)
	if err != nil {
		log.Printf("[StopVM] error: vmID=%s, mode=%s, err=%v", vmID, forceStr, err)
	} else {
		log.Printf("[StopVM] success: vmID=%s, mode=%s", vmID, forceStr)
	}
	return err
}

func (s *VMService) RestartVM(ctx context.Context, vmID string) error {
	log.Printf("[RestartVM] vmID=%s", vmID)
	err := s.mgr.RestartVM(ctx, vmID)
	if err != nil {
		log.Printf("[RestartVM] error: vmID=%s, err=%v", vmID, err)
	} else {
		log.Printf("[RestartVM] success: vmID=%s", vmID)
	}
	return err
}

func (s *VMService) DeleteVM(ctx context.Context, vmID string) error {
	log.Printf("[DeleteVM] vmID=%s", vmID)
	err := s.mgr.DeleteVM(ctx, vmID)
	if err != nil {
		log.Printf("[DeleteVM] error: vmID=%s, err=%v", vmID, err)
	} else {
		log.Printf("[DeleteVM] success: vmID=%s", vmID)
	}
	return err
}

// --- 配置与维护 ---

func (s *VMService) ReinstallVM(ctx context.Context, req *agent.CmdReinstallVM) error {
	// 驱动层会处理特定驱动配置的复用与重新分配，并负责保存 VMConfig
	return s.mgr.ReinstallVM(ctx, req)
}

func (s *VMService) ResetPassword(ctx context.Context, vmID, password string) error {
	return s.mgr.ResetPassword(ctx, vmID, password)
}

// --- 信息查询 ---

func (s *VMService) GetVMInfo(ctx context.Context, vmID string) (*agent.VMSummary, error) {
	info, err := s.mgr.GetVMInfo(ctx, vmID)
	if err != nil {
		return nil, err
	}
	info.VmId = vmID
	return info, nil
}

func (s *VMService) ListVMs(ctx context.Context) ([]*agent.VMSummary, error) {
	all, err := s.mgr.ListVMs(ctx)
	if err != nil {
		return nil, err
	}

	// 仅返回 DB 中存在的 VM，排除非平台管理的实例
	configs, _ := s.db.ListVMConfigs()
	registered := make(map[string]bool, len(configs))
	for _, c := range configs {
		registered[c.VMID] = true
	}

	var result []*agent.VMSummary
	for _, vm := range all {
		if registered[vm.VmId] {
			result = append(result, vm)
		}
	}
	return result, nil
}

// AttachTTY 将终端附接请求转发给底层驱动。
func (s *VMService) AttachTTY(ctx context.Context, vmID string, stdin io.Reader, stdout io.Writer, resize <-chan ResizeEvent) error {
	return s.mgr.AttachTTY(ctx, vmID, stdin, stdout, resize)
}

// --- 其他 ---

func (s *VMService) GetVMLocalIP(ctx context.Context, vmID string) (string, error) {
	return s.mgr.GetVMLocalIP(ctx, vmID)
}

func (s *VMService) GetSupportedImages(ctx context.Context) ([]*agent.OSImageInfo, error) {
	return s.mgr.GetSupportedImages(ctx)
}

// GetVMNetStats 获取 VM 的网络流量统计（用于流量统计服务）
func (s *VMService) GetVMNetStats(ctx context.Context, vmID string) (*VMNetStats, error) {
	return s.mgr.GetVMNetStats(ctx, vmID)
}

// Autostart 启动所有状态为 running 的 VM（通常在 agent 启动时调用）
func (s *VMService) Autostart(ctx context.Context) {
	configs, _ := s.db.ListVMConfigs()
	for _, c := range configs {
		if c.Status == "running" {
			// 忽略日志输出，交由调用方处理
			_ = s.mgr.StartVM(ctx, c.VMID)
		}
	}
}

// Cleanup 执行幽灵实例清理
func (s *VMService) Cleanup(ctx context.Context) error {
	return s.mgr.Cleanup(ctx)
}
