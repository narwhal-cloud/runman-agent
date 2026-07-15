package manager

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"

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

	// 仅返回 DB 中存在的 VM，排除非平台管理的实例。
	// DB 出错时返回错误而非空列表，避免心跳上报 0 个 VM（会触发后端误判逻辑）。
	configs, err := s.db.ListVMConfigs()
	if err != nil {
		return nil, fmt.Errorf("list VM configs: %w", err)
	}
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

// SupportsCustomImages 返回底层驱动是否支持自定义镜像（即实现了 ImagePuller）。
func (s *VMService) SupportsCustomImages() bool {
	_, ok := s.mgr.(ImagePuller)
	return ok
}

// StartCustomImagePull 后台拉取自定义镜像并更新其在 DB 中的状态。
func (s *VMService) StartCustomImagePull(ref string) {
	puller, ok := s.mgr.(ImagePuller)
	if !ok {
		_ = s.db.UpdateCustomImageStatus(ref, db.CustomImageError, "driver does not support custom images")
		return
	}
	go func() {
		log.Printf("[CustomImage] pulling %s ...", ref)
		if err := puller.PullImage(context.Background(), ref); err != nil {
			log.Printf("[CustomImage] pull %s failed: %v", ref, err)
			_ = s.db.UpdateCustomImageStatus(ref, db.CustomImageError, err.Error())
			return
		}
		log.Printf("[CustomImage] pull %s done", ref)
		_ = s.db.UpdateCustomImageStatus(ref, db.CustomImageReady, "")
	}()
}

// ResumeCustomImagePulls 恢复因 agent 重启而中断的拉取任务（启动时调用一次）。
func (s *VMService) ResumeCustomImagePulls() {
	for _, img := range s.db.ListCustomImages() {
		if img.Status == db.CustomImagePulling {
			s.StartCustomImagePull(img.ID)
		}
	}
}

// GetVMNetStats 获取 VM 的网络流量统计（用于流量统计服务）
func (s *VMService) GetVMNetStats(ctx context.Context, vmID string) (*VMNetStats, error) {
	return s.mgr.GetVMNetStats(ctx, vmID)
}

// Autostart 启动所有状态为 running 的 VM（通常在 agent 启动时调用）
func (s *VMService) Autostart(ctx context.Context) {
	configs, err := s.db.ListVMConfigs()
	if err != nil {
		log.Printf("[Autostart] failed to list VM configs: %v", err)
		return
	}
	for _, c := range configs {
		if c.Status == "running" {
			if err := s.mgr.StartVM(ctx, c.VMID); err != nil {
				log.Printf("[Autostart] failed to start VM %s: %v", c.VMID, err)
			} else {
				log.Printf("[Autostart] started VM %s", c.VMID)
			}
		}
	}
}

// Cleanup 执行幽灵实例清理
func (s *VMService) Cleanup(ctx context.Context) error {
	return s.mgr.Cleanup(ctx)
}

// FilterAndSortImages 根据数据库中的 os_images_config 配置对镜像列表进行过滤和排序。
func FilterAndSortImages(d *db.DB, images []*agent.OSImageInfo) []*agent.OSImageInfo {
	configStr, err := d.GetSystem("os_images_config")
	if err != nil || configStr == "" {
		return images
	}

	// 解析配置列表，如 "alpine,debian" -> ["alpine", "debian"]
	var config []string
	for _, part := range strings.Split(configStr, ",") {
		part = strings.TrimSpace(strings.ToLower(part))
		if part != "" {
			config = append(config, part)
		}
	}

	// 如果配置为空列表，即全取消了，返回空列表
	if len(config) == 0 {
		return []*agent.OSImageInfo{}
	}

	// 建立一个 map，分类为 "debian" 和 "alpine"
	categoryMap := make(map[string]*agent.OSImageInfo)
	for _, img := range images {
		lowerId := strings.ToLower(img.Id)
		if strings.Contains(lowerId, "debian") {
			categoryMap["debian"] = img
		} else if strings.Contains(lowerId, "alpine") {
			categoryMap["alpine"] = img
		}
	}

	// 按照配置顺序组装最终列表
	var result []*agent.OSImageInfo
	for _, cat := range config {
		if img, ok := categoryMap[cat]; ok {
			result = append(result, img)
		}
	}

	return result
}

// AppendReadyCustomImages 将 DB 中已就绪（ready）的自定义镜像追加到镜像列表末尾。
// 自定义镜像不参与 FilterAndSortImages 的 debian/alpine 分类过滤，应在过滤之后调用。
func AppendReadyCustomImages(d *db.DB, images []*agent.OSImageInfo) []*agent.OSImageInfo {
	seen := make(map[string]bool, len(images))
	for _, img := range images {
		seen[img.Id] = true
	}
	for _, ci := range d.ListCustomImages() {
		if ci.Status != db.CustomImageReady || seen[ci.ID] {
			continue
		}
		name := ci.Name
		if name == "" {
			name = ci.ID
		}
		images = append(images, &agent.OSImageInfo{Id: ci.ID, Name: name})
	}
	return images
}
