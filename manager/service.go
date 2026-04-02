package manager

import (
	"context"
	"fmt"
	"io"
	"net"
	"runman-agent/db"
	"runman-agent/proto/agent"

	"google.golang.org/protobuf/proto"
)

// GenerateMACFromIP 根据 IP 地址确定性地生成 MAC 地址
// MAC 前缀固定为 52:54:00:00:01，最后一个八位字节从 IP 的最后一个八位字节导出
func GenerateMACFromIP(ipStr string) string {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return ""
	}
	// 获取 IPv4 的最后一个八位字节
	ipv4 := ip.To4()
	if ipv4 == nil {
		return ""
	}
	lastOctet := ipv4[3]
	// 生成 MAC 地址：52:54:00:00:01:XX（XX 是 IP 的最后一个八位字节）
	return fmt.Sprintf("52:54:00:00:01:%02x", lastOctet)
}

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

// localID 将平台 VM ID 转换为底层虚拟化驱动识别的标识符。
func (s *VMService) localID(vmID string) string {
	if conf, err := s.db.GetVMConfig(vmID); err == nil {
		return conf.LocalID
	}
	return vmID
}

// --- 生命周期管理 ---

func (s *VMService) CreateVM(ctx context.Context, req *agent.CmdCreateVM) error {
	if err := s.mgr.CreateVM(ctx, req); err != nil {
		return err
	}

	// 驱动层已经负责将 IP/MAC/Cpuset 等底层配置存入数据库。
	// 这里补全业务层的其他配置字段。
	conf, _ := s.db.GetVMConfig(req.VmId)
	if conf != nil {
		conf.LocalID = req.VmId
		conf.BandwidthMbps = int(req.BandwidthMbps)
		conf.CPU = int(req.Cpu)
		conf.MemoryMB = req.RamMb
		conf.Image = req.OsImage
		conf.Status = "running"
		_ = s.db.SaveVMConfig(conf)
	}

	if s.OnCreated != nil {
		s.OnCreated(ctx, req.VmId, int(req.BandwidthMbps))
	}
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
	return s.mgr.DeleteVM(ctx, s.localID(vmID))
}

// --- 配置与维护 ---

func (s *VMService) UpdateVM(ctx context.Context, vmID string, cpu int32, ramMB int64, diskGB int64, bandwidthMBPS int32) error {
	return s.mgr.UpdateVM(ctx, s.localID(vmID), cpu, ramMB, diskGB, bandwidthMBPS)
}

func (s *VMService) ReinstallVM(ctx context.Context, req *agent.CmdReinstallVM) error {
	localID := s.localID(req.VmId)
	r := proto.Clone(req).(*agent.CmdReinstallVM)
	r.VmId = localID
	// 驱动层会处理 IP/MAC/Cpuset 的复用与重新分配
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

// AttachTTY 将终端附接请求转发给底层驱动。
func (s *VMService) AttachTTY(ctx context.Context, vmID string, stdin io.Reader, stdout io.Writer, resize <-chan ResizeEvent) error {
	return s.mgr.AttachTTY(ctx, s.localID(vmID), stdin, stdout, resize)
}

// --- 其他 ---

func (s *VMService) GetVMIP(ctx context.Context, vmID string) (string, error) {
	return s.mgr.GetVMIP(ctx, s.localID(vmID))
}

func (s *VMService) GetVMMAC(ctx context.Context, vmID string) (string, error) {
	return s.mgr.GetVMMAC(ctx, s.localID(vmID))
}

func (s *VMService) GetSupportedImages(ctx context.Context) ([]*agent.OSImageInfo, error) {
	return s.mgr.GetSupportedImages(ctx)
}
