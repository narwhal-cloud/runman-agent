package manager

import (
	"context"
	"runman-agent/proto/agent"
)

// cpusetKey 是向 context 注入 cpuset 字符串时使用的键。
// 由 VMService 写入，由底层驱动（如 podman.Manager）读取。
type cpusetKey struct{}

// CpusetKey 供底层驱动从 context 中读取分配好的 cpuset。
var CpusetKey = cpusetKey{}

// WithCpuset 将 cpuset 注入 context。
func WithCpuset(ctx context.Context, cpuset string) context.Context {
	return context.WithValue(ctx, CpusetKey, cpuset)
}

// CpusetFrom 从 context 中取出 cpuset，不存在时返回空字符串。
func CpusetFrom(ctx context.Context) string {
	v, _ := ctx.Value(CpusetKey).(string)
	return v
}

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
