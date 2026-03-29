package db

import (
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

type Config struct {
	ID            uint   `gorm:"primaryKey"`
	ServerAddr    string `json:"server_addr"`
	Token         string `json:"token"`
	MonitorNIC    string `json:"monitor_nic"`    // 指定监控网卡
	MonitorDisk   string `json:"monitor_disk"`   // 指定监控磁盘/挂载点
	VirtType      string `json:"virt_type"`      // 固定虚拟化类型 (podman/kvm)
	BandwidthMbps int32  `json:"bandwidth_mbps"` // 启动测速结果 (Mbps)
	WebUser       string `json:"web_user"`       // 面板用户名
	WebPassHash   string `json:"-"`              // bcrypt hash，不暴露到 API
}

type VMConfig struct {
	VMID string `gorm:"primaryKey"`
	// LocalID 是底层虚拟化系统中实际使用的标识符（podman 容器名、KVM 虚拟机 UUID 等）
	LocalID       string
	BandwidthMbps int
	CPU           int
	MemoryMB      int64
	Image         string
	Status        string
	Cpuset        string // 分配给该 VM 的 cpuset，如 "0-2" 或 "0,3,5"
	MAC           string // 固定的MAC地址
	IP            string // 固定的IP地址
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
	_ = db.AutoMigrate(&Traffic{}, &VMConfig{}, &Config{}, &PortForward{})

	// Ensure default config exists
	var count int64
	db.Model(&Config{}).Count(&count)
	if count == 0 {
		db.Create(&Config{ServerAddr: "localhost:9090"})
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

// VM 配置管理
func (d *DB) SaveVMConfig(v *VMConfig) error {
	return d.orm.Save(v).Error
}

func (d *DB) GetVMConfig(vmId string) (*VMConfig, error) {
	var conf VMConfig
	err := d.orm.First(&conf, "vm_id = ?", vmId).Error
	return &conf, err
}

func (d *DB) ListVMConfigs() ([]*VMConfig, error) {
	var list []*VMConfig
	err := d.orm.Find(&list).Error
	return list, err
}

func (d *DB) DeleteVMConfig(vmId string) error {
	return d.orm.Delete(&VMConfig{}, "vm_id = ?", vmId).Error
}

// 端口转发持久化
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

// UpdateTraffic 流量统计逻辑
func (d *DB) UpdateTraffic(vmId string, rawIn, rawOut int64, month string) (totalIn, totalOut, monthIn, monthOut int64, err error) {
	var t Traffic
	err = d.orm.First(&t, "vm_id = ?", vmId).Error
	if err != nil {
		t = Traffic{VMID: vmId, RawIn: rawIn, RawOut: rawOut, Month: month}
		d.orm.Create(&t)
	}

	diffIn := rawIn - t.RawIn
	if diffIn < 0 {
		diffIn = 0
	}
	diffOut := rawOut - t.RawOut
	if diffOut < 0 {
		diffOut = 0
	}

	t.RawIn = rawIn
	t.RawOut = rawOut
	t.TotalIn += diffIn
	t.TotalOut += diffOut

	if t.Month != month {
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
