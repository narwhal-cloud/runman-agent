package manager

import (
	"context"
	"io"
	"runman-agent/proto/agent"
)

// ResizeEvent 表示终端尺寸变更事件，由 Web 层通过 WebSocket 控制消息触发。
type ResizeEvent struct {
	Cols uint
	Rows uint
}

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

// macAddressKey 用于向 context 注入 MAC 地址。
// 由 VMService 或调用者写入，由底层驱动读取。
type macAddressKey struct{}

var MacAddressKey = macAddressKey{}

// WithMacAddress 将 MAC 地址注入 context。
func WithMacAddress(ctx context.Context, mac string) context.Context {
	return context.WithValue(ctx, MacAddressKey, mac)
}

// MacAddressFrom 从 context 中取出 MAC 地址，不存在时返回空字符串。
func MacAddressFrom(ctx context.Context) string {
	v, _ := ctx.Value(MacAddressKey).(string)
	return v
}

// ipAddressKey 用于向 context 注入 IP 地址。
// 由 VMService 或调用者写入，由底层驱动读取。
type ipAddressKey struct{}

var IPAddressKey = ipAddressKey{}

// WithIPAddress 将 IP 地址注入 context。
func WithIPAddress(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, IPAddressKey, ip)
}

// IPAddressFrom 从 context 中取出 IP 地址，不存在时返回空字符串。
func IPAddressFrom(ctx context.Context) string {
	v, _ := ctx.Value(IPAddressKey).(string)
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
	UpdateVM(ctx context.Context, vmID string, cpu int32, ramMB int64, diskGB int64, bandwidthMBPS int32) error
	ReinstallVM(ctx context.Context, req *agent.CmdReinstallVM) error
	ResetPassword(ctx context.Context, vmID, password string) error

	// GetVMInfo 信息查询
	GetVMInfo(ctx context.Context, vmID string) (*agent.VMSummary, error)
	ListVMs(ctx context.Context) ([]*agent.VMSummary, error)

	// GetVMIP 获取 VM 内部 IP (用于端口转发)
	GetVMIP(ctx context.Context, vmID string) (string, error)

	// GetVMMAC 获取 VM MAC 地址
	GetVMMAC(ctx context.Context, vmID string) (string, error)

	// GetSupportedImages 获取该虚拟化支持的镜像列表
	GetSupportedImages(ctx context.Context) ([]*agent.OSImageInfo, error)

	// AttachTTY 附接到虚拟机/容器的交互式终端。
	// stdin 来自调用方，stdout 输出回调用方。
	// resize 通道用于传递终端尺寸变更事件，关闭后不再发送。
	// 函数阻塞直到会话结束或 ctx 被取消。
	AttachTTY(ctx context.Context, vmID string, stdin io.Reader, stdout io.Writer, resize <-chan ResizeEvent) error
}
