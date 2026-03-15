package db

import (
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type Config struct {
	ID          uint   `gorm:"primaryKey"`
	ServerAddr  string `json:"server_addr"`
	Token       string `json:"token"`
	MonitorNIC  string `json:"monitor_nic"`  // 指定监控网卡
	MonitorDisk string `json:"monitor_disk"` // 指定监控磁盘/挂载点
	VirtType    string `json:"virt_type"`    // 固定虚拟化类型 (podman/kvm)
}

type VMConfig struct {
	VMID          string `gorm:"primaryKey"`
	BandwidthMbps int
	CPU           int
	MemoryMB      int64
	Image         string
	Status        string
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
	_ = db.AutoMigrate(&Traffic{}, &VMConfig{}, &Config{})

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

// 流量统计逻辑
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
