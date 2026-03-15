package manager

import (
	"context"
	"runman-agent/proto/agent"
)

// VMManager 定义了虚拟化管理器的通用接口
type VMManager interface {
	// CreateVM 基础生命周期管理
	CreateVM(ctx context.Context, req *agent.CmdCreateVM) error
	StartVM(ctx context.Context, vmID string) error
	StopVM(ctx context.Context, vmID string, force bool) error
	RestartVM(ctx context.Context, vmID string) error
	DeleteVM(ctx context.Context, vmID string) error

	// UpdateVM 配置与维护
	UpdateVM(ctx context.Context, req *agent.CmdUpdateVM) error
	ReinstallVM(ctx context.Context, req *agent.CmdReinstallVM) error
	ResetPassword(ctx context.Context, vmID, password string) error

	// GetVMInfo 信息查询
	GetVMInfo(ctx context.Context, vmID string) (*agent.VMSummary, error)
	ListVMs(ctx context.Context) ([]*agent.VMSummary, error)

	// GetVMIP 获取 VM 内部 IP (用于端口转发)
	GetVMIP(ctx context.Context, vmID string) (string, error)

	// GetSupportedImages 获取该虚拟化支持的镜像列表
	GetSupportedImages(ctx context.Context) ([]*agent.OSImageInfo, error)
}
