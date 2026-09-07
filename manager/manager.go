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

// macAddressKey 用于向 context 注入 MAC 地址。
// 已废弃：现在由底层驱动直接管理。
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
// 已废弃：现在由底层驱动直接管理。
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

	// 配置与维护
	ReinstallVM(ctx context.Context, req *agent.CmdReinstallVM) error
	ResetPassword(ctx context.Context, vmID, password string) error

	// GetVMInfo 信息查询
	GetVMInfo(ctx context.Context, vmID string) (*agent.VMSummary, error)
	ListVMs(ctx context.Context) ([]*agent.VMSummary, error)

	// GetVMLocalIP 获取 VM 的可达内部地址（IPv4 优先，无 IPv4 时可返回 IPv6）
	// 用于用户态端口转发。
	GetVMLocalIP(ctx context.Context, vmID string) (string, error)

	// GetSupportedImages 获取该虚拟化支持的镜像列表
	GetSupportedImages(ctx context.Context) ([]*agent.OSImageInfo, error)

	// AttachTTY 附接到虚拟机/容器的交互式终端。
	// stdin 来自调用方，stdout 输出回调用方。
	// resize 通道用于传递终端尺寸变更事件，关闭后不再发送。
	// 函数阻塞直到会话结束或 ctx 被取消。
	AttachTTY(ctx context.Context, vmID string, stdin io.Reader, stdout io.Writer, resize <-chan ResizeEvent) error

	// GetVMNetStats 获取 VM 的网络流量累计字节数（用于流量统计服务）
	// 返回的是计数器值，而非速率，由调用方计算增量
	GetVMNetStats(ctx context.Context, vmID string) (*VMNetStats, error)

	// Cleanup 清理幽灵实例（存在于驱动中但不在数据库中的实例）
	Cleanup(ctx context.Context) error
}

// ImagePuller 是可选能力接口：支持按引用预拉取镜像的驱动（目前仅 podman）实现它。
type ImagePuller interface {
	PullImage(ctx context.Context, ref string) error
}

// VMNetStats 表示 VM 的网络流量统计
type VMNetStats struct {
	VMID     string // VM ID
	InBytes  int64  // 累计入站字节数（计数器值）
	OutBytes int64  // 累计出站字节数（计数器值）
}
