// Package wgbind 实现纯用户态的 WireGuard 绑定：为某个 VM 挂一条或多条 WG 隧道，
// 隧道地址上的全部 TCP/UDP 流量按原端口号转发进该 VM 的内网 IPv4，发往隧道地址的
// ping 由 Agent 自己应答。
//
// 整条链路不依赖内核 WireGuard 模块、不建 TUN 设备、不写 iptables/nftables 规则，
// 因此可以在没有 CAP_NET_ADMIN 的环境里跑，也不会和母鸡上其它网络配置打架。
// 唯一需要的权限是绑定一个 UDP 端口（>=1024 时连 root 都不需要）。
//
// 已知边界：只处理入站方向。VM 主动发起的连接仍然走母鸡的内核路由出去，
// 不会被 SNAT 成隧道地址——那需要在 VM 的网络命名空间里做策略路由，
// 纯用户态方案做不到。
package wgbind

import (
	"context"
	"fmt"
	"log"
	"runman-agent/db"
	"runman-agent/manager"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// retryInterval 是启动失败的绑定的重试间隔（常见原因：UDP 端口被占用、
// 母鸡刚重启时网络还没就绪）。
const retryInterval = 30 * time.Second

type Manager struct {
	mu       sync.RWMutex
	bindings map[string]*binding // key: binding ID，只含成功跑起来的
	failed   map[string]string   // key: binding ID → 最近一次启动失败原因

	vmMgr manager.VMManager
	db    *db.DB

	// ctx 是所有隧道的父 context，随 Manager 一生不变（单条隧道的关停走 binding.cancel），
	// 因此可以被 HTTP handler 起的 Add/Update 无锁读取。
	ctx context.Context
}

// New 创建 Manager。ctx 用于在进程退出时统一收敛所有隧道与后台循环。
func New(ctx context.Context, vmMgr manager.VMManager, database *db.DB) *Manager {
	return &Manager{
		bindings: make(map[string]*binding),
		failed:   make(map[string]string),
		vmMgr:    vmMgr,
		db:       database,
		ctx:      ctx,
	}
}

// Restore 在启动时把 DB 里已启用的绑定全部拉起，并开启后台重试循环。
func (m *Manager) Restore() {
	all, err := m.db.ListAllWGBindings()
	if err != nil {
		log.Printf("[WGBind] restore: list bindings failed: %v", err)
		return
	}
	for _, b := range all {
		if !b.Enabled {
			continue
		}
		if err := m.start(*b); err != nil {
			log.Printf("[WGBind] restore %s (%s) failed (will retry): %v", shortID(b.ID), b.Address, err)
		} else {
			log.Printf("[WGBind] restored %s: %s -> VM %s", shortID(b.ID), b.Address, b.VMID)
		}
	}
	go m.retryLoop(m.ctx)
}

// retryLoop 周期性重试启动失败的绑定。
func (m *Manager) retryLoop(ctx context.Context) {
	ticker := time.NewTicker(retryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		all, err := m.db.ListAllWGBindings()
		if err != nil {
			continue
		}
		for _, b := range all {
			if !b.Enabled {
				continue
			}
			m.mu.RLock()
			_, running := m.bindings[b.ID]
			m.mu.RUnlock()
			if running {
				continue
			}
			if err := m.start(*b); err == nil {
				log.Printf("[WGBind] retry: recovered %s (%s)", shortID(b.ID), b.Address)
			}
		}
	}
}

// start 校验配置并拉起一条绑定。cfg 按值传入，内部会被 validate 补全默认值。
func (m *Manager) start(cfg db.WGBinding) error {
	before := cfg
	norm, err := validate(&cfg)
	if err != nil {
		m.noteFailure(cfg.ID, err)
		return err
	}
	// validate 会补全默认值（MTU、AllowedIPs、主动方的 keepalive……）。把补全后的
	// 配置写回 DB，保证"面板上显示的"就是"实际生效的"——老记录升级上来时尤其重要。
	if cfg != before {
		_ = m.db.SaveWGBinding(&cfg)
	}

	b, err := startBinding(m.ctx, m, cfg, norm)
	if err != nil {
		m.noteFailure(cfg.ID, err)
		return err
	}

	m.mu.Lock()
	if old := m.bindings[cfg.ID]; old != nil {
		old.stop()
	}
	m.bindings[cfg.ID] = b
	delete(m.failed, cfg.ID)
	m.mu.Unlock()
	return nil
}

func (m *Manager) noteFailure(id string, err error) {
	m.mu.Lock()
	m.failed[id] = err.Error()
	m.mu.Unlock()
}

// stopLocked 停掉一条运行中的绑定。调用方需持有 m.mu 写锁。
func (m *Manager) stopLocked(id string) {
	if b := m.bindings[id]; b != nil {
		b.stop()
		delete(m.bindings, id)
	}
}

// Add 新建一条绑定。配置非法时不落库，直接返回错误让面板显示。
func (m *Manager) Add(vmID string, cfg db.WGBinding) (*db.WGBinding, error) {
	cfg.ID = uuid.NewString()
	cfg.VMID = vmID
	cfg.CreatedAt = time.Now()

	// 先校验（validate 会就地补全 MTU / AllowedIPs 等默认值），保证入库的记录
	// 和实际生效的配置一致。
	if _, err := validate(&cfg); err != nil {
		return nil, err
	}
	if err := m.checkConflict(&cfg); err != nil {
		return nil, err
	}

	if err := m.db.SaveWGBinding(&cfg); err != nil {
		return nil, err
	}
	if cfg.Enabled {
		if err := m.start(cfg); err != nil {
			// 已落库，面板上会显示为 error 状态并由 retryLoop 继续重试，
			// 这里把错误一并返回让用户立刻看到原因。
			return &cfg, err
		}
	}
	return &cfg, nil
}

// Update 覆盖一条已有绑定的配置，并重建隧道。
func (m *Manager) Update(id string, cfg db.WGBinding) (*db.WGBinding, error) {
	old, err := m.db.GetWGBinding(id)
	if err != nil {
		return nil, fmt.Errorf("binding %s not found", id)
	}
	cfg.ID = old.ID
	cfg.VMID = old.VMID
	cfg.CreatedAt = old.CreatedAt

	if _, err := validate(&cfg); err != nil {
		return nil, err
	}
	if err := m.checkConflict(&cfg); err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.stopLocked(id)
	delete(m.failed, id)
	m.mu.Unlock()

	if err := m.db.SaveWGBinding(&cfg); err != nil {
		return nil, err
	}
	if cfg.Enabled {
		if err := m.start(cfg); err != nil {
			return &cfg, err
		}
	}
	return &cfg, nil
}

// checkConflict 拦截会导致启动必然失败或行为诡异的重复配置。
func (m *Manager) checkConflict(cfg *db.WGBinding) error {
	all, err := m.db.ListAllWGBindings()
	if err != nil {
		return err
	}
	for _, o := range all {
		if o.ID == cfg.ID {
			continue
		}
		if o.Address == cfg.Address {
			return fmt.Errorf("tunnel address %s is already used by another binding", cfg.Address)
		}
		if cfg.ListenPort != 0 && o.ListenPort == cfg.ListenPort && o.Enabled {
			return fmt.Errorf("listen port %d is already used by another binding", cfg.ListenPort)
		}
	}
	return nil
}

// Remove 删除一条绑定（停隧道 + 删记录）。
func (m *Manager) Remove(id string) error {
	m.mu.Lock()
	m.stopLocked(id)
	delete(m.failed, id)
	m.mu.Unlock()
	return m.db.DeleteWGBinding(id)
}

// DeleteVM 删除某 VM 的全部绑定，供 VM 被删除时调用。
func (m *Manager) DeleteVM(vmID string) {
	list, err := m.db.ListWGBindings(vmID)
	if err != nil {
		return
	}
	m.mu.Lock()
	for _, b := range list {
		m.stopLocked(b.ID)
		delete(m.failed, b.ID)
	}
	m.mu.Unlock()
	_ = m.db.DeleteWGBindingsForVM(vmID)
}

// RefreshVM 在 VM 重装/重启后丢弃其绑定缓存的 VM IP，使下一条连接重新解析。
// 隧道本身不用重建——它和 VM 的生命周期是解耦的。
func (m *Manager) RefreshVM(vmID string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, b := range m.bindings {
		if b.cfg.VMID == vmID {
			b.invalidateIP()
		}
	}
}

// Status 是一条绑定对外暴露的完整状态（配置 + 运行态 + 统计）。
// PrivateKey 不在其中，只回一个由它推导出的 PublicKey 供用户填到对端。
type Status struct {
	ID        string `json:"id"`
	VMID      string `json:"vm_id"`
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
	Address   string `json:"address"`
	PublicKey string `json:"public_key"` // 本端公钥，由私钥推导

	ListenPort    int    `json:"listen_port"`
	MTU           int    `json:"mtu"`
	PeerPublicKey string `json:"peer_public_key"`
	HasPSK        bool   `json:"has_psk"`
	Endpoint      string `json:"endpoint"`
	AllowedIPs    string `json:"allowed_ips"`
	Keepalive     int    `json:"keepalive"`

	// 运行态
	State        string `json:"state"`          // running | stopped | error
	Error        string `json:"error"`          // state=error 时的原因
	LastDialErr  string `json:"last_dial_err"`  // 最近一次拨向 VM 失败的原因
	ActualPort   int    `json:"actual_port"`    // 实际监听的 UDP 端口（配 0 时为随机值）
	PeerEndpoint string `json:"peer_endpoint"`  // 对端当前地址（被动模式下由握手学到）
	LastHands    int64  `json:"last_handshake"` // Unix 秒，0 = 从未握手
	RxBytes      int64  `json:"rx_bytes"`
	TxBytes      int64  `json:"tx_bytes"`

	TargetIP    string    `json:"target_ip"` // 当前解析到的 VM 内网 IPv4
	ActiveConns int64     `json:"active_conns"`
	TotalConns  int64     `json:"total_conns"`
	TopIPs      []IPCount `json:"top_ips"`
}

// List 返回某 VM 的全部绑定状态；vmID 为空时返回所有 VM 的。
func (m *Manager) List(vmID string) ([]Status, error) {
	var records []*db.WGBinding
	var err error
	if vmID == "" {
		records, err = m.db.ListAllWGBindings()
	} else {
		records, err = m.db.ListWGBindings(vmID)
	}
	if err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]Status, 0, len(records))
	for _, rec := range records {
		s := Status{
			ID:            rec.ID,
			VMID:          rec.VMID,
			Name:          rec.Name,
			Enabled:       rec.Enabled,
			Address:       rec.Address,
			ListenPort:    rec.ListenPort,
			MTU:           rec.MTU,
			PeerPublicKey: rec.PeerPublicKey,
			HasPSK:        rec.PresharedKey != "",
			Endpoint:      rec.Endpoint,
			AllowedIPs:    rec.AllowedIPs,
			Keepalive:     rec.Keepalive,
			TopIPs:        []IPCount{},
		}
		if pub, err := PublicKeyOf(rec.PrivateKey); err == nil {
			s.PublicKey = pub
		}

		b := m.bindings[rec.ID]
		switch {
		case b != nil:
			s.State = "running"
			ps := b.peerStats()
			s.ActualPort = ps.ListenPort
			s.PeerEndpoint = ps.Endpoint
			if !ps.LastHandshake.IsZero() {
				s.LastHands = ps.LastHandshake.Unix()
			}
			s.RxBytes, s.TxBytes = ps.RxBytes, ps.TxBytes
			s.ActiveConns = atomicLoad(&b.connActive)
			s.TotalConns = atomicLoad(&b.connTotal)
			s.TopIPs = b.topIPs(10)
			b.ipMu.Lock()
			s.TargetIP = b.cachedIP
			b.ipMu.Unlock()
			b.errMu.Lock()
			s.LastDialErr = b.lastErr
			b.errMu.Unlock()
		case !rec.Enabled:
			s.State = "stopped"
		default:
			s.State = "error"
			s.Error = m.failed[rec.ID]
		}
		out = append(out, s)
	}
	return out, nil
}

// Get 返回单条绑定的原始记录（含私钥），供更新时做部分字段合并。
func (m *Manager) Get(id string) (*db.WGBinding, error) {
	return m.db.GetWGBinding(id)
}

func atomicLoad(p *int64) int64 { return atomic.LoadInt64(p) }
