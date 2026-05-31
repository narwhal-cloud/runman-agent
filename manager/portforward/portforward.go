package portforward

import (
	"context"
	"fmt"
	"io"
	"net"
	"runman-agent/config"
	"runman-agent/db"
	"runman-agent/manager"
	"sync"
	"time"
)

const (
	MinPort = 1024
	MaxPort = 65535
)

// --- 转发管理实现 ---

type Entry struct {
	VMID        string
	Protocol    string // "tcp" or "udp"
	HostPort    int
	GuestPort   int
	TargetAddr  string // resolved "ip:port" at setup time, for diagnostics
	Description string
	cancel      context.CancelFunc
	ln          io.Closer
}

// DesiredRule 用于 SyncForVM 全量同步时描述期望状态
type DesiredRule struct {
	Protocol    string
	HostPort    int
	GuestPort   int
	Description string
}

type Manager struct {
	mu       sync.RWMutex
	mappings map[string][]*Entry
	mgr      manager.VMManager
	db       *db.DB
	cfg      *config.Manager
}

func New(mgr manager.VMManager, database *db.DB, cfg *config.Manager) *Manager {
	return &Manager{
		mappings: make(map[string][]*Entry),
		mgr:      mgr,
		db:       database,
		cfg:      cfg,
	}
}

// Restore 启动时从 DB 恢复所有持久化的端口转发规则。
func (m *Manager) Restore(ctx context.Context) {
	all, err := m.db.ListAllPortForwards()
	if err != nil {
		return
	}
	for _, pf := range all {
		_ = m.addMapping(ctx, pf.VMID, pf.Protocol, pf.HostPort, pf.GuestPort, pf.Description, false)
	}
}

// RefreshVM 重新解析 VM 的 IP 并重建其所有端口转发规则。
// 用于 VM 重装后 IP 可能发生变化的场景。
func (m *Manager) RefreshVM(ctx context.Context, vmID string) {
	rules, err := m.db.ListPortForwards(vmID)
	if err != nil || len(rules) == 0 {
		return
	}
	// 先全部移除（释放 listener），再重新 addMapping（重新调 GetVMIP）
	for _, r := range rules {
		_ = m.RemoveMapping(ctx, vmID, r.Protocol, r.HostPort)
	}
	for _, r := range rules {
		_ = m.addMapping(ctx, vmID, r.Protocol, r.HostPort, r.GuestPort, r.Description, false)
	}
}

// AddMapping 添加转发规则，相同规则幂等，配置变更时先删后加
func (m *Manager) AddMapping(ctx context.Context, vmId string, protocol string, hostPort, guestPort int, description string) error {
	return m.addMapping(ctx, vmId, protocol, hostPort, guestPort, description, true)
}

func (m *Manager) addMapping(ctx context.Context, vmId string, protocol string, hostPort, guestPort int, description string, checkLimit bool) error {
	if hostPort < MinPort || hostPort > MaxPort {
		return fmt.Errorf("host port %d out of allowed range [%d, %d]", hostPort, MinPort, MaxPort)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	exists := false
	for _, e := range m.mappings[vmId] {
		if e.Protocol == protocol && e.HostPort == hostPort {
			if e.GuestPort == guestPort && e.Description == description {
				return nil
			}
			m.removeEntryLocked(vmId, protocol, hostPort)
			exists = true
			break
		}
	}

	if checkLimit && !exists {
		maxPF := int(m.cfg.Get().MaxPortForward)
		if len(m.mappings[vmId]) >= maxPF {
			return fmt.Errorf("port forward limit reached (%d/%d)", len(m.mappings[vmId]), maxPF)
		}
	}

	ip, err := m.mgr.GetVMLocalIP(ctx, vmId)
	if err != nil {
		return err
	}
	if ip == "" {
		return fmt.Errorf("VM %s has no IP address (is it running?)", vmId)
	}
	targetAddr := fmt.Sprintf("%s:%d", ip, guestPort)

	runCtx, cancel := context.WithCancel(context.Background())
	entry := &Entry{
		VMID:        vmId,
		Protocol:    protocol,
		HostPort:    hostPort,
		GuestPort:   guestPort,
		TargetAddr:  targetAddr,
		Description: description,
		cancel:      cancel,
	}

	if protocol == "tcp" {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", hostPort))
		if err != nil {
			cancel()
			return err
		}
		entry.ln = ln
		go m.runTCP(runCtx, ln, targetAddr)
	} else {
		pc, err := net.ListenPacket("udp", fmt.Sprintf(":%d", hostPort))
		if err != nil {
			cancel()
			return err
		}
		entry.ln = pc
		go m.runUDP(runCtx, pc, targetAddr)
	}

	m.mappings[vmId] = append(m.mappings[vmId], entry)
	_ = m.db.SavePortForward(&db.PortForward{
		Protocol:    protocol,
		HostPort:    hostPort,
		VMID:        vmId,
		GuestPort:   guestPort,
		Description: description,
	})
	return nil
}

// removeEntryLocked removes the entry matching (vmId, protocol, hostPort) from
// the in-memory map and the database. Caller must hold m.mu (write lock).
func (m *Manager) removeEntryLocked(vmId, protocol string, hostPort int) {
	entries := m.mappings[vmId]
	for i, e := range entries {
		if e.Protocol == protocol && e.HostPort == hostPort {
			e.cancel()
			if e.ln != nil {
				_ = e.ln.Close()
			}
			m.mappings[vmId] = append(entries[:i], entries[i+1:]...)
			_ = m.db.DeletePortForward(protocol, hostPort)
			return
		}
	}
}

func (m *Manager) RemoveMapping(_ context.Context, vmId string, protocol string, hostPort int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removeEntryLocked(vmId, protocol, hostPort)
	return nil
}

// SyncForVM 以 desired 为准全量对齐某个 VM 的端口转发规则
func (m *Manager) SyncForVM(ctx context.Context, vmId string, desired []DesiredRule) error {
	m.mu.RLock()
	current := make([]*Entry, len(m.mappings[vmId]))
	copy(current, m.mappings[vmId])
	m.mu.RUnlock()

	// 删除不在期望列表中的规则
	for _, c := range current {
		found := false
		for _, d := range desired {
			if c.Protocol == d.Protocol && c.HostPort == d.HostPort {
				found = true
				break
			}
		}
		if !found {
			_ = m.RemoveMapping(ctx, vmId, c.Protocol, c.HostPort)
		}
	}

	// 添加或更新期望的规则
	for _, d := range desired {
		_ = m.AddMapping(ctx, vmId, d.Protocol, d.HostPort, d.GuestPort, d.Description)
	}

	return nil
}

// DeleteVM 删除某 VM 的所有转发规则（内存 + DB）
func (m *Manager) DeleteVM(ctx context.Context, vmId string) {
	m.mu.RLock()
	entries := make([]*Entry, len(m.mappings[vmId]))
	copy(entries, m.mappings[vmId])
	m.mu.RUnlock()
	for _, e := range entries {
		_ = m.RemoveMapping(ctx, vmId, e.Protocol, e.HostPort)
	}
	_ = m.db.DeletePortForwardsForVM(vmId)
}

func (m *Manager) GetReport() []Entry {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var all []Entry
	for _, entries := range m.mappings {
		for _, e := range entries {
			all = append(all, Entry{
				VMID:        e.VMID,
				Protocol:    e.Protocol,
				HostPort:    e.HostPort,
				GuestPort:   e.GuestPort,
				TargetAddr:  e.TargetAddr,
				Description: e.Description,
			})
		}
	}
	return all
}

// --- 内部转发逻辑 ---

func (m *Manager) runTCP(ctx context.Context, ln net.Listener, target string) {
	defer func() { _ = ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				time.Sleep(time.Second)
				continue
			}
		}
		go m.handleTCP(ctx, conn, target)
	}
}

func (m *Manager) handleTCP(ctx context.Context, src net.Conn, target string) {
	defer func() { _ = src.Close() }()
	dst, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		return
	}
	defer func() { _ = dst.Close() }()

	done := make(chan struct{}, 2)
	go func() {
		m.proxy(dst, src)
		done <- struct{}{}
	}()
	go func() {
		m.proxy(src, dst)
		done <- struct{}{}
	}()

	select {
	case <-ctx.Done():
	case <-done:
	}
}

func (m *Manager) runUDP(ctx context.Context, pc net.PacketConn, target string) {
	defer func() { _ = pc.Close() }()
	targetAddr, _ := net.ResolveUDPAddr("udp", target)
	sessions := make(map[string]net.Conn)
	var smu sync.Mutex

	buf := make([]byte, 64*1024)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				continue
			}
		}

		smu.Lock()
		client, ok := sessions[addr.String()]
		if !ok {
			var dialErr error
			client, dialErr = net.Dial("udp", targetAddr.String())
			if dialErr != nil {
				smu.Unlock()
				continue
			}
			sessions[addr.String()] = client
			go func(a net.Addr, c net.Conn) {
				defer func() {
					smu.Lock()
					delete(sessions, a.String())
					smu.Unlock()
					_ = c.Close()
				}()
				rBuf := make([]byte, 64*1024)
				for {
					_ = c.SetReadDeadline(time.Now().Add(60 * time.Second))
					rn, err := c.Read(rBuf)
					if err != nil {
						return
					}
					_, _ = pc.WriteTo(rBuf[:rn], a)
				}
			}(addr, client)
		}
		smu.Unlock()

		_, _ = client.Write(buf[:n])
	}
}

func (m *Manager) proxy(dst io.Writer, src io.Reader) {
	_, _ = io.Copy(dst, src)
}
