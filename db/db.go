package db

import (
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

const DefaultMaxPortForward = 20

type Config struct {
	ID             uint   `gorm:"primaryKey"`
	Token          string `json:"token"`
	MonitorNIC     string `json:"monitor_nic"`      // 指定监控网卡
	MonitorDisk    string `json:"monitor_disk"`     // 指定监控磁盘/挂载点
	VirtType       string `json:"virt_type"`        // 固定虚拟化类型 (podman/kvm)
	BandwidthMbps  int32  `json:"bandwidth_mbps"`   // 启动测速结果 (Mbps)
	WebUser        string `json:"web_user"`         // 面板用户名
	WebPassHash    string `json:"-"`                // bcrypt hash，不暴露到 API
	Host           string `json:"host"`             // 上报给服务端的入口地址（IPv4/DDNS），空则由服务端自取
	MaxPortForward int32  `json:"max_port_forward"` // 每个 VM 最大转发端口数
}

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

// PortForward 持久化端口转发规则。(Protocol, HostPort) 联合主键保证宿主机端口唯一。
type PortForward struct {
	Protocol    string `gorm:"primaryKey"`
	HostPort    int    `gorm:"primaryKey"`
	VMID        string `gorm:"index"`
	GuestPort   int
	Description string
}

type Traffic struct {
	VMID     string `gorm:"primaryKey"`
	RawIn    int64
	RawOut   int64
	TotalIn  int64
	TotalOut int64
	Month    string `gorm:"index"` // YYYY-MM
	MonthIn  int64
	MonthOut int64
}

type DB struct {
	orm *gorm.DB
}

func Init(path string) (*DB, error) {
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		return nil, err
	}
	_ = db.AutoMigrate(&Traffic{}, &VMConfig{}, &PodmanVMConfig{}, &Config{}, &PortForward{})

	// Ensure default config exists
	var conf Config
	err = db.First(&conf).Error
	if err != nil {
		// No config found, create default
		db.Create(&Config{MaxPortForward: DefaultMaxPortForward})
	} else if conf.MaxPortForward == 0 {
		// Config exists but MaxPortForward is unset, update it
		conf.MaxPortForward = DefaultMaxPortForward
		db.Save(&conf)
	}

	return &DB{orm: db}, nil
}

func (d *DB) GetConfig() (*Config, error) {
	var conf Config
	err := d.orm.First(&conf).Error
	return &conf, err
}

func (d *DB) SaveConfig(c *Config) error {
	return d.orm.Save(c).Error
}

// VM 核心业务配置

func (d *DB) SaveVMConfig(v *VMConfig) error {
	return d.orm.Save(v).Error
}

func (d *DB) GetVMConfig(vmId string) (*VMConfig, error) {
	var conf VMConfig
	err := d.orm.First(&conf, "vmid = ?", vmId).Error
	return &conf, err
}

func (d *DB) ListVMConfigs() ([]*VMConfig, error) {
	var list []*VMConfig
	err := d.orm.Find(&list).Error
	return list, err
}

func (d *DB) DeleteVMConfig(vmId string) error {
	return d.orm.Delete(&VMConfig{}, "vmid = ?", vmId).Error
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

// Podman数据结构

func (d *DB) SavePodmanConfig(v *PodmanVMConfig) error {
	return d.orm.Save(v).Error
}

func (d *DB) GetPodmanConfig(vmId string) (*PodmanVMConfig, error) {
	var conf PodmanVMConfig
	err := d.orm.First(&conf, "vmid = ?", vmId).Error
	return &conf, err
}

func (d *DB) DeletePodmanConfig(vmId string) error {
	return d.orm.Delete(&PodmanVMConfig{}, "vmid = ?", vmId).Error
}
func (d *DB) ListPodmanConfigs() ([]*PodmanVMConfig, error) {
	var list []*PodmanVMConfig
	err := d.orm.Find(&list).Error
	return list, err
}
