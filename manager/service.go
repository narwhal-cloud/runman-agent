package manager

import (
	"context"
	"runman-agent/db"
	"runman-agent/proto/agent"

	"google.golang.org/protobuf/proto"
)

// VMService 是 VMManager 的服务层包装，位于 gRPC 处理层与底层虚拟化驱动之间，负责：
//  1. 将平台下发的 serverVmID 转换为底层虚拟化驱动实际使用的 localID
//  2. ListVMs 只返回本节点 DB 中已登记的 VM，过滤用户自行创建的容器/虚拟机
//  3. 为未来多虚拟化类型的数据兼容提供统一入口
type VMService struct {
	mgr VMManager
	db  *db.DB
}

func NewVMService(mgr VMManager, database *db.DB) *VMService {
	return &VMService{mgr: mgr, db: database}
}

// localID 将平台 VM ID 转换为底层虚拟化驱动识别的标识符。
// 若 DB 中查不到记录，直接使用平台 ID（防御性回退，正常情况不应触发）。
func (s *VMService) localID(vmID string) string {
	if conf, err := s.db.GetVMConfig(vmID); err == nil {
		return conf.LocalID
	}
	return vmID
}

// --- 生命周期管理 ---

func (s *VMService) CreateVM(ctx context.Context, req *agent.CmdCreateVM) error {
	// CreateVM 不做 ID 转换：底层驱动以 req.VmId 为名创建资源，
	// LocalID 由调用方在创建成功后写入 DB。
	return s.mgr.CreateVM(ctx, req)
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
	return s.mgr.DeleteVM(ctx, s.localID(vmID))
}

// --- 配置与维护 ---

func (s *VMService) UpdateVM(ctx context.Context, req *agent.CmdUpdateVM) error {
	localID := s.localID(req.VmId)
	if localID == req.VmId {
		return s.mgr.UpdateVM(ctx, req)
	}
	r := proto.Clone(req).(*agent.CmdUpdateVM)
	r.VmId = localID
	return s.mgr.UpdateVM(ctx, r)
}

func (s *VMService) ReinstallVM(ctx context.Context, req *agent.CmdReinstallVM) error {
	localID := s.localID(req.VmId)
	if localID == req.VmId {
		return s.mgr.ReinstallVM(ctx, req)
	}
	r := proto.Clone(req).(*agent.CmdReinstallVM)
	r.VmId = localID
	return s.mgr.ReinstallVM(ctx, r)
}

func (s *VMService) ResetPassword(ctx context.Context, vmID, password string) error {
	return s.mgr.ResetPassword(ctx, s.localID(vmID), password)
}

// --- 信息查询 ---

// GetVMInfo 返回的 VmId 始终是平台侧的 serverVmID，屏蔽底层 localID 差异。
func (s *VMService) GetVMInfo(ctx context.Context, vmID string) (*agent.VMSummary, error) {
	info, err := s.mgr.GetVMInfo(ctx, s.localID(vmID))
	if err != nil {
		return nil, err
	}
	info.VmId = vmID
	return info, nil
}

// ListVMs 仅返回本节点 DB 中已登记的 VM，并将底层 localID 映射回 serverVmID。
// 用户在宿主机上自行创建的容器/虚拟机不会出现在结果中。
func (s *VMService) ListVMs(ctx context.Context) ([]*agent.VMSummary, error) {
	all, err := s.mgr.ListVMs(ctx)
	if err != nil {
		return nil, err
	}

	// 从 DB 构建 localID → serverVmID 的映射表
	configs, _ := s.db.ListVMConfigs()
	localToServer := make(map[string]string, len(configs))
	for _, c := range configs {
		localToServer[c.LocalID] = c.VMID
	}

	var result []*agent.VMSummary
	for _, vm := range all {
		serverID, ok := localToServer[vm.VmId]
		if !ok {
			continue // 非本节点托管的虚拟机，跳过
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
