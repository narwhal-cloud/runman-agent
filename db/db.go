package db

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type VMConfig struct {
	VMID          string `gorm:"primaryKey"`
	CPU           int
	MemoryMB      int64
	DiskGB        int64
	BandwidthMbps int
	Image         string
	Status        string
}

// PodmanVMConfig 存储 Podman 驱动特有的数据
type PodmanVMConfig struct {
	VMID      string `gorm:"primaryKey"`
	Container string // 容器名
	Cpuset    string // 分配的 cpuset
	MAC       string // 固定 MAC
	IPv4      string // 本地ipv4一定存在
	IPv6      string // 公网ipv6可能存在
}

// CloudHVVMConfig 存储 cloud-hypervisor 驱动特有的数据
type CloudHVVMConfig struct {
	VMID string `gorm:"primaryKey"`
	Idx  int    // IP/TAP 索引，范围 2-4094
	MAC  string // 固定 MAC 地址
	IPv4 string // 静态 IPv4，来自 10.91.0.0/20 段
	IPv6 string // 静态 IPv6（ULA 或公网 /112），可能为空
}

// IncusVMConfig 存储 Incus 驱动特有的数据
type IncusVMConfig struct {
	VMID      string `gorm:"primaryKey"`
	Idx       int    // IP 索引，用于计算静态 IP
	Container string // 容器名
	Image     string // 镜像名
	IPv4      string // 静态 IPv4
	IPv6      string // 静态 IPv6（多地址时取首个，便于兼容旧逻辑/NDP 主地址）
	IPv6s     string // 静态 IPv6 列表，逗号分隔（多地址精细化分配时 >1 个）
}

// VMIPLimit 按 VM 覆盖端口转发唯一来源 IP 数上限。
// 无记录时使用全局配置 MaxForwardIPs；MaxIPs 为 0 表示该 VM 不限制。
type VMIPLimit struct {
	VMID   string `gorm:"primaryKey"`
	MaxIPs int
}

// WGBinding 持久化一条 WireGuard 绑定。每条绑定 = 一个纯用户态 WG 隧道，
// 隧道地址（单个 IPv4 或 IPv6）上收到的全部 TCP/UDP 流量按原端口号转发到该 VM 的内网 IPv4。
// 密钥以 base64 存储，与 wg 配置文件的书写形式一致。
type WGBinding struct {
	ID            string `gorm:"primaryKey"` // uuid
	VMID          string `gorm:"index"`
	Name          string // 面向用户的备注名
	Enabled       bool
	PrivateKey    string // 本端私钥
	Address       string // 隧道地址，单个 IPv4/IPv6
	ListenPort    int    // 本端 WG UDP 监听端口，0 = 随机（仅在配了 Endpoint 时允许）
	MTU           int    // 0 = DefaultMTU
	PeerPublicKey string
	PresharedKey  string // 可空
	Endpoint      string // 对端 host:port，可空表示被动等待对端连入
	AllowedIPs    string // 逗号分隔，空 = 0.0.0.0/0, ::/0
	Keepalive     int    // PersistentKeepalive 秒数，0 = 不发
	CreatedAt     time.Time
}

// PortForward 持久化端口转发规则。(Protocol, HostPort) 联合主键保证宿主机端口唯一。
type PortForward struct {
	Protocol    string `gorm:"primaryKey"`
	HostPort    int    `gorm:"primaryKey"`
	VMID        string `gorm:"index"`
	GuestPort   int
	Description string
}

type Traffic struct {
	VMID      string    `gorm:"primaryKey"`
	RawIn     int64     // 最后一次采集的网卡计数器值（入站）
	RawOut    int64     // 最后一次采集的网卡计数器值（出站）
	TotalIn   int64     // 累计使用量（字节）
	TotalOut  int64     // 累计使用量（字节）
	Month     string    `gorm:"index"` // YYYY-MM
	MonthIn   int64     // 当月使用量（字节）
	MonthOut  int64     // 当月使用量（字节）
	UpdatedAt time.Time // 最后同步时间
}

// System 存储 Agent 系统级别的元数据，如检测到新版本的时间
type System struct {
	Key       string `gorm:"primaryKey"`
	Value     string
	UpdatedAt time.Time
}

type DB struct {
	orm *gorm.DB
	// ciMu 序列化自定义镜像列表的读改写，防止并发拉取状态更新互相覆盖
	ciMu sync.Mutex
}

func Init(path string) (*DB, error) {
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{
		Logger: logger.New(log.New(os.Stdout, "\r\n", log.LstdFlags), logger.Config{
			SlowThreshold:             200 * time.Millisecond,
			LogLevel:                  logger.Error,
			IgnoreRecordNotFoundError: true,
			Colorful:                  true,
		}),
	})
	if err != nil {
		return nil, err
	}

	// SQLite 并发安全配置：
	// WAL 模式让读操作不会被写操作阻塞（写时仍可读）；
	// busy_timeout 避免并发写时立即返回 SQLITE_BUSY 错误（等待 5s 再报错）；
	// synchronous=NORMAL 在 WAL 模式下已足够安全，比 FULL 快；
	// SetMaxOpenConns(1) 保证所有 goroutine 串行使用同一个连接，彻底消除并发写冲突。
	db.Exec("PRAGMA journal_mode = WAL")
	db.Exec("PRAGMA busy_timeout = 5000")
	db.Exec("PRAGMA synchronous = NORMAL")
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(1)
	}

	if err := db.AutoMigrate(&Traffic{}, &VMConfig{}, &PodmanVMConfig{}, &CloudHVVMConfig{}, &IncusVMConfig{}, &PortForward{}, &VMIPLimit{}, &WGBinding{}, &System{}); err != nil {
		return nil, fmt.Errorf("automigrate: %w", err)
	}

	return &DB{orm: db}, nil
}

// System 元数据

func (d *DB) GetSystem(key string) (string, error) {
	var s System
	err := d.orm.First(&s, "key = ?", key).Error
	if err != nil {
		return "", err
	}
	return s.Value, nil
}

func (d *DB) SetSystem(key, value string) error {
	return d.orm.Save(&System{Key: key, Value: value, UpdatedAt: time.Now()}).Error
}

func (d *DB) GetSystemTime(key string) (time.Time, error) {
	var s System
	err := d.orm.First(&s, "key = ?", key).Error
	if err != nil {
		return time.Time{}, err
	}
	return s.UpdatedAt, nil
}

// 自定义镜像（仅 podman 驱动使用），JSON 序列化后存于 System KV

const customImagesKey = "custom_images"

// CustomImage 拉取状态
const (
	CustomImagePulling = "pulling"
	CustomImageReady   = "ready"
	CustomImageError   = "error"
)

type CustomImage struct {
	ID      string `json:"id"`   // 完整镜像引用，如 docker.io/user/image:tag
	Name    string `json:"name"` // 面向用户的显示名
	Status  string `json:"status"`
	Error   string `json:"error,omitempty"`
	AddedAt int64  `json:"added_at"`
}

func (d *DB) loadCustomImagesLocked() []CustomImage {
	raw, err := d.GetSystem(customImagesKey)
	if err != nil || raw == "" {
		return nil
	}
	var list []CustomImage
	if json.Unmarshal([]byte(raw), &list) != nil {
		return nil
	}
	return list
}

func (d *DB) saveCustomImagesLocked(list []CustomImage) error {
	raw, err := json.Marshal(list)
	if err != nil {
		return err
	}
	return d.SetSystem(customImagesKey, string(raw))
}

func (d *DB) ListCustomImages() []CustomImage {
	d.ciMu.Lock()
	defer d.ciMu.Unlock()
	return d.loadCustomImagesLocked()
}

// UpsertCustomImage 新增或更新一条自定义镜像记录（按 ID 匹配）。
func (d *DB) UpsertCustomImage(img CustomImage) error {
	d.ciMu.Lock()
	defer d.ciMu.Unlock()
	list := d.loadCustomImagesLocked()
	for i := range list {
		if list[i].ID == img.ID {
			list[i] = img
			return d.saveCustomImagesLocked(list)
		}
	}
	return d.saveCustomImagesLocked(append(list, img))
}

func (d *DB) DeleteCustomImage(id string) error {
	d.ciMu.Lock()
	defer d.ciMu.Unlock()
	list := d.loadCustomImagesLocked()
	out := list[:0]
	for _, img := range list {
		if img.ID != id {
			out = append(out, img)
		}
	}
	return d.saveCustomImagesLocked(out)
}

// UpdateCustomImageStatus 更新指定镜像的拉取状态；记录不存在时（如拉取期间被删除）静默忽略。
func (d *DB) UpdateCustomImageStatus(id, status, errMsg string) error {
	d.ciMu.Lock()
	defer d.ciMu.Unlock()
	list := d.loadCustomImagesLocked()
	for i := range list {
		if list[i].ID == id {
			list[i].Status = status
			list[i].Error = errMsg
			return d.saveCustomImagesLocked(list)
		}
	}
	return nil
}

// VM 核心业务配置

func (d *DB) SaveVMConfig(v *VMConfig) error {
	return d.orm.Save(v).Error
}

func (d *DB) GetVMConfig(vmId string) (*VMConfig, error) {
	var conf VMConfig
	err := d.orm.First(&conf, "vm_id = ?", vmId).Error
	if err != nil {
		return nil, err
	}
	return &conf, nil
}

func (d *DB) ListVMConfigs() ([]*VMConfig, error) {
	var list []*VMConfig
	err := d.orm.Find(&list).Error
	return list, err
}

func (d *DB) DeleteVMConfig(vmId string) error {
	return d.orm.Delete(&VMConfig{}, "vm_id = ?", vmId).Error
}

// 端口转发

func (d *DB) SavePortForward(pf *PortForward) error {
	return d.orm.Save(pf).Error
}

func (d *DB) DeletePortForward(protocol string, hostPort int) error {
	return d.orm.Delete(&PortForward{}, "protocol = ? AND host_port = ?", protocol, hostPort).Error
}

func (d *DB) ListPortForwards(vmId string) ([]*PortForward, error) {
	var list []*PortForward
	err := d.orm.Where("vm_id = ?", vmId).Find(&list).Error
	return list, err
}

func (d *DB) ListAllPortForwards() ([]*PortForward, error) {
	var list []*PortForward
	err := d.orm.Find(&list).Error
	return list, err
}

func (d *DB) DeletePortForwardsForVM(vmId string) error {
	return d.orm.Delete(&PortForward{}, "vm_id = ?", vmId).Error
}

// WireGuard 绑定

func (d *DB) SaveWGBinding(b *WGBinding) error {
	return d.orm.Save(b).Error
}

func (d *DB) GetWGBinding(id string) (*WGBinding, error) {
	var b WGBinding
	err := d.orm.First(&b, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (d *DB) DeleteWGBinding(id string) error {
	return d.orm.Delete(&WGBinding{}, "id = ?", id).Error
}

func (d *DB) ListWGBindings(vmId string) ([]*WGBinding, error) {
	var list []*WGBinding
	err := d.orm.Where("vm_id = ?", vmId).Order("created_at").Find(&list).Error
	return list, err
}

func (d *DB) ListAllWGBindings() ([]*WGBinding, error) {
	var list []*WGBinding
	err := d.orm.Order("created_at").Find(&list).Error
	return list, err
}

func (d *DB) DeleteWGBindingsForVM(vmId string) error {
	return d.orm.Delete(&WGBinding{}, "vm_id = ?", vmId).Error
}

// 端口转发唯一来源 IP 数限制（按 VM 覆盖）

func (d *DB) SaveVMIPLimit(vmId string, maxIPs int) error {
	return d.orm.Save(&VMIPLimit{VMID: vmId, MaxIPs: maxIPs}).Error
}

func (d *DB) DeleteVMIPLimit(vmId string) error {
	return d.orm.Delete(&VMIPLimit{}, "vm_id = ?", vmId).Error
}

func (d *DB) ListVMIPLimits() ([]*VMIPLimit, error) {
	var list []*VMIPLimit
	err := d.orm.Find(&list).Error
	return list, err
}

// 流量

func (d *DB) DeleteTraffic(vmId string) error {
	return d.orm.Delete(&Traffic{}, "vm_id = ?", vmId).Error
}

func (d *DB) GetTraffic(vmId string) (*Traffic, error) {
	var t Traffic
	err := d.orm.First(&t, "vm_id = ?", vmId).Error
	return &t, err
}

func (d *DB) UpdateTraffic(vmId string, rawIn, rawOut int64, month string) (totalIn, totalOut, monthIn, monthOut int64, err error) {
	var t Traffic
	err = d.orm.First(&t, "vm_id = ?", vmId).Error
	if err != nil {
		// 首次看到该 VM，将当前流量作为初始值计入统计
		t = Traffic{
			VMID:     vmId,
			RawIn:    rawIn,
			RawOut:   rawOut,
			TotalIn:  rawIn,
			TotalOut: rawOut,
			Month:    month,
			MonthIn:  rawIn,
			MonthOut: rawOut,
		}
		d.orm.Create(&t)
		return t.TotalIn, t.TotalOut, t.MonthIn, t.MonthOut, nil
	}

	// 计算增量
	diffIn := rawIn - t.RawIn
	if diffIn < 0 {
		// 计数器重置（如容器/宿主机重启），将当前 raw 值全量计入增量
		diffIn = rawIn
	}
	diffOut := rawOut - t.RawOut
	if diffOut < 0 {
		diffOut = rawOut
	}

	t.RawIn = rawIn
	t.RawOut = rawOut
	t.TotalIn += diffIn
	t.TotalOut += diffOut

	if t.Month != month {
		// 跨月重置
		t.Month = month
		t.MonthIn = diffIn
		t.MonthOut = diffOut
	} else {
		t.MonthIn += diffIn
		t.MonthOut += diffOut
	}

	d.orm.Save(&t)
	return t.TotalIn, t.TotalOut, t.MonthIn, t.MonthOut, nil
}

// SaveTraffic 直接保存或更新 Traffic 记录（用于流量统计服务）
func (d *DB) SaveTraffic(t *Traffic) error {
	return d.orm.Save(t).Error
}

// ResetTrafficMonth 重置虚拟机的当月流量为0
func (d *DB) ResetTrafficMonth(vmId string) error {
	return d.orm.Model(&Traffic{}).Where("vm_id = ?", vmId).Updates(map[string]interface{}{
		"month_in":  0,
		"month_out": 0,
	}).Error
}

// Podman数据结构

func (d *DB) SavePodmanConfig(v *PodmanVMConfig) error {
	return d.orm.Save(v).Error
}

func (d *DB) GetPodmanConfig(vmId string) (*PodmanVMConfig, error) {
	var conf PodmanVMConfig
	err := d.orm.First(&conf, "vm_id = ?", vmId).Error
	if err != nil {
		return nil, err
	}
	return &conf, nil
}

func (d *DB) DeletePodmanConfig(vmId string) error {
	return d.orm.Delete(&PodmanVMConfig{}, "vm_id = ?", vmId).Error
}
func (d *DB) ListPodmanConfigs() ([]*PodmanVMConfig, error) {
	var list []*PodmanVMConfig
	err := d.orm.Find(&list).Error
	return list, err
}

// cloud-hypervisor 数据结构

func (d *DB) SaveCloudHVConfig(v *CloudHVVMConfig) error {
	return d.orm.Save(v).Error
}

func (d *DB) GetCloudHVConfig(vmId string) (*CloudHVVMConfig, error) {
	var conf CloudHVVMConfig
	err := d.orm.First(&conf, "vm_id = ?", vmId).Error
	if err != nil {
		return nil, err
	}
	return &conf, nil
}

func (d *DB) DeleteCloudHVConfig(vmId string) error {
	return d.orm.Delete(&CloudHVVMConfig{}, "vm_id = ?", vmId).Error
}

func (d *DB) ListCloudHVConfigs() ([]*CloudHVVMConfig, error) {
	var list []*CloudHVVMConfig
	err := d.orm.Find(&list).Error
	return list, err
}

// Incus 数据结构

func (d *DB) SaveIncusConfig(v *IncusVMConfig) error {
	return d.orm.Save(v).Error
}

func (d *DB) GetIncusConfig(vmId string) (*IncusVMConfig, error) {
	var conf IncusVMConfig
	err := d.orm.First(&conf, "vm_id = ?", vmId).Error
	if err != nil {
		return nil, err
	}
	return &conf, nil
}

func (d *DB) DeleteIncusConfig(vmId string) error {
	return d.orm.Delete(&IncusVMConfig{}, "vm_id = ?", vmId).Error
}

func (d *DB) ListIncusConfigs() ([]*IncusVMConfig, error) {
	var list []*IncusVMConfig
	err := d.orm.Find(&list).Error
	return list, err
}

// NextIncusIdx 返回最小可用的 idx（范围 2-4094）
func (d *DB) NextIncusIdx() (int, error) {
	var configs []*IncusVMConfig
	if err := d.orm.Find(&configs).Error; err != nil {
		return 2, err
	}

	used := make(map[int]bool, len(configs))
	for _, c := range configs {
		used[c.Idx] = true
	}

	for idx := 2; idx <= 4094; idx++ {
		if !used[idx] {
			return idx, nil
		}
	}
	return -1, gorm.ErrRecordNotFound
}

// NextCloudHVIdx 返回最小可用的 idx（范围 2-4094）
func (d *DB) NextCloudHVIdx() (int, error) {
	var configs []*CloudHVVMConfig
	if err := d.orm.Find(&configs).Error; err != nil {
		return 2, err
	}

	used := make(map[int]bool, len(configs))
	for _, c := range configs {
		used[c.Idx] = true
	}

	for idx := 2; idx <= 4094; idx++ {
		if !used[idx] {
			return idx, nil
		}
	}
	return -1, gorm.ErrRecordNotFound // 没有可用的 idx
}
