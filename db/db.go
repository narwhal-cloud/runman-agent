package db

import (
	"log"
	"os"
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

type DB struct {
	orm *gorm.DB
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
	_ = db.AutoMigrate(&Traffic{}, &VMConfig{}, &PodmanVMConfig{}, &CloudHVVMConfig{}, &PortForward{})

	return &DB{orm: db}, nil
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
