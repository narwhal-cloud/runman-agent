package manager

import (
	"context"
	"runman-agent/db"
	"runman-agent/manager/cpualloc"
	"runman-agent/proto/agent"

	"google.golang.org/protobuf/proto"
)

// VMService 是 VMManager 的服务层包装，位于 gRPC 处理层与底层虚拟化驱动之间，负责：
//  1. 将平台下发的 serverVmID 转换为底层虚拟化驱动实际使用的 localID
//  2. ListVMs 只返回本节点 DB 中已登记的 VM，过滤用户自行创建的容器/虚拟机
//  3. 管理 CPU 分配：CreateVM 时分配 cpuset，DeleteVM 时释放，UpdateVM 时重新分配
type VMService struct {
	mgr   VMManager
	db    *db.DB
	alloc *cpualloc.Allocator
}

func NewVMService(mgr VMManager, database *db.DB, alloc *cpualloc.Allocator) *VMService {
	return &VMService{mgr: mgr, db: database, alloc: alloc}
}

// localID 将平台 VM ID 转换为底层虚拟化驱动识别的标识符。
func (s *VMService) localID(vmID string) string {
	if conf, err := s.db.GetVMConfig(vmID); err == nil {
		return conf.LocalID
	}
	return vmID
}

// --- 生命周期管理 ---

func (s *VMService) CreateVM(ctx context.Context, req *agent.CmdCreateVM) error {
	cpuset, err := s.alloc.Allocate(int(req.Cpu))
	if err != nil {
		return err
	}
	ctx = WithCpuset(ctx, cpuset)
	if err = s.mgr.CreateVM(ctx, req); err != nil {
		s.alloc.Release(cpuset)
		return err
	}
	// cpuset 持久化到 DB，由调用方（main.go handleCommand）在 SaveVMConfig 时写入
	// 这里通过 context 传回，调用方从 ctx 中读取
	return nil
}

func (s *VMService) StartVM(ctx context.Context, vmID string) error {
	return s.mgr.StartVM(ctx, s.localID(vmID))
}

func (s *VMService) StopVM(ctx context.Context, vmID string, force bool) error {
	return s.mgr.StopVM(ctx, s.localID(vmID), force)
}

func (s *VMService) RestartVM(ctx context.Context, vmID string) error {
	return s.mgr.RestartVM(ctx, s.localID(vmID))
}

func (s *VMService) DeleteVM(ctx context.Context, vmID string) error {
	// 释放 cpuset 引用
	if conf, err := s.db.GetVMConfig(vmID); err == nil {
		s.alloc.Release(conf.Cpuset)
	}
	return s.mgr.DeleteVM(ctx, s.localID(vmID))
}

// --- 配置与维护 ---

func (s *VMService) UpdateVM(ctx context.Context, req *agent.CmdUpdateVM) error {
	localID := s.localID(req.VmId)

	// CPU 数量变化时重新分配 cpuset
	if req.Cpu > 0 {
		if conf, err := s.db.GetVMConfig(req.VmId); err == nil {
			s.alloc.Release(conf.Cpuset)
		}
		cpuset, err := s.alloc.Allocate(int(req.Cpu))
		if err != nil {
			return err
		}
		ctx = WithCpuset(ctx, cpuset)
		// 将新 cpuset 持久化
		if conf, err := s.db.GetVMConfig(req.VmId); err == nil {
			conf.Cpuset = cpuset
			_ = s.db.SaveVMConfig(conf)
		}
	}

	r := proto.Clone(req).(*agent.CmdUpdateVM)
	r.VmId = localID
	return s.mgr.UpdateVM(ctx, r)
}

func (s *VMService) ReinstallVM(ctx context.Context, req *agent.CmdReinstallVM) error {
	// 重装保持原有 CPU 数量，重新分配 cpuset
	if conf, err := s.db.GetVMConfig(req.VmId); err == nil {
		s.alloc.Release(conf.Cpuset)
		cpuset, allocErr := s.alloc.Allocate(conf.CPU)
		if allocErr == nil {
			ctx = WithCpuset(ctx, cpuset)
			conf.Cpuset = cpuset
			_ = s.db.SaveVMConfig(conf)
		}
	} else if req.Cpu > 0 {
		// 母鸡重装系统后 DB 配置丢失，使用下发参数中的 CPU 数量分配 cpuset
		cpuset, allocErr := s.alloc.Allocate(int(req.Cpu))
		if allocErr == nil {
			ctx = WithCpuset(ctx, cpuset)
			// 预存最小记录，让 main.go 的 GetVMConfig 能读到 cpuset
			_ = s.db.SaveVMConfig(&db.VMConfig{
				VMID:    req.VmId,
				LocalID: req.VmId,
				CPU:     int(req.Cpu),
				Cpuset:  cpuset,
			})
		}
	}

	localID := s.localID(req.VmId)
	r := proto.Clone(req).(*agent.CmdReinstallVM)
	r.VmId = localID
	return s.mgr.ReinstallVM(ctx, r)
}

func (s *VMService) ResetPassword(ctx context.Context, vmID, password string) error {
	return s.mgr.ResetPassword(ctx, s.localID(vmID), password)
}

// --- 信息查询 ---

func (s *VMService) GetVMInfo(ctx context.Context, vmID string) (*agent.VMSummary, error) {
	info, err := s.mgr.GetVMInfo(ctx, s.localID(vmID))
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

	configs, _ := s.db.ListVMConfigs()
	localToServer := make(map[string]string, len(configs))
	for _, c := range configs {
		localToServer[c.LocalID] = c.VMID
	}

	var result []*agent.VMSummary
	for _, vm := range all {
		serverID, ok := localToServer[vm.VmId]
		if !ok {
			continue
		}
		vm.VmId = serverID
		result = append(result, vm)
	}
	return result, nil
}

// --- 其他 ---

func (s *VMService) GetVMIP(ctx context.Context, vmID string) (string, error) {
	return s.mgr.GetVMIP(ctx, s.localID(vmID))
}

func (s *VMService) GetSupportedImages(ctx context.Context) ([]*agent.OSImageInfo, error) {
	return s.mgr.GetSupportedImages(ctx)
}
