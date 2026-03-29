package portforward

import (
	"context"
	"fmt"
	"io"
	"net"
	"runman-agent/db"
	"runman-agent/manager"
	"sync"
	"time"
)

// --- 限速器实现 ---

type RateLimiter struct {
	rate       int64
	capacity   int64
	tokens     int64
	lastUpdate time.Time
	mu         sync.Mutex
}

func NewRateLimiter(bytesPerSec int64) *RateLimiter {
	if bytesPerSec <= 0 {
		return nil
	}
	return &RateLimiter{
		rate:       bytesPerSec,
		capacity:   bytesPerSec,
		tokens:     bytesPerSec,
		lastUpdate: time.Now(),
	}
}

func (l *RateLimiter) Wait(n int) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	for {
		now := time.Now()
		elapsed := now.Sub(l.lastUpdate).Seconds()
		l.tokens += int64(elapsed * float64(l.rate))
		if l.tokens > l.capacity {
			l.tokens = l.capacity
		}
		l.lastUpdate = now

		if l.tokens >= int64(n) {
			l.tokens -= int64(n)
			return
		}

		needed := int64(n) - l.tokens
		waitTime := time.Duration(float64(needed)/float64(l.rate)*1e9) * time.Nanosecond
		l.mu.Unlock()
		time.Sleep(waitTime)
		l.mu.Lock()
	}
}

type LimitedWriter struct {
	w       io.Writer
	limiter *RateLimiter
}

func (lw *LimitedWriter) Write(p []byte) (n int, err error) {
	lw.limiter.Wait(len(p))
	return lw.w.Write(p)
}

// --- 转发管理实现 ---

type Entry struct {
	VMID        string
	Protocol    string // "tcp" or "udp"
	HostPort    int
	GuestPort   int
	Mbps        int
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
}

func New(mgr manager.VMManager, database *db.DB) *Manager {
	return &Manager{
		mappings: make(map[string][]*Entry),
		mgr:      mgr,
		db:       database,
	}
}

// Restore 启动时从 DB 恢复所有持久化的端口转发规则。
func (m *Manager) Restore(ctx context.Context) {
	all, err := m.db.ListAllPortForwards()
	if err != nil {
		return
	}
	for _, pf := range all {
		_ = m.AddMapping(ctx, pf.VMID, pf.Protocol, pf.HostPort, pf.GuestPort, 0, pf.Description)
	}
}

// RefreshVM 重新解析 VM 的 IP 并重建其所有端口转发规则。
// 用于 VM 重装后 IP 可能发生变化的场景。
func (m *Manager) RefreshVM(ctx context.Context, vmID string) {
	rules, err := m.db.ListPortForwards(vmID)
	if err != nil || len(rules) == 0 {
		return
	}
	// 先全部移除（释放 listener），再重新 AddMapping（重新调 GetVMIP）
	for _, r := range rules {
		_ = m.RemoveMapping(ctx, vmID, r.Protocol, r.HostPort)
	}
	for _, r := range rules {
		_ = m.AddMapping(ctx, vmID, r.Protocol, r.HostPort, r.GuestPort, 0, r.Description)
	}
}

// AddMapping 添加转发规则，相同规则幂等，配置变更时先删后加
func (m *Manager) AddMapping(ctx context.Context, vmId string, protocol string, hostPort, guestPort int, mbps int, description string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, e := range m.mappings[vmId] {
		if e.Protocol == protocol && e.HostPort == hostPort {
			if e.GuestPort == guestPort && e.Mbps == mbps && e.Description == description {
				return nil
			}
			m.mu.Unlock()
			_ = m.RemoveMapping(ctx, vmId, protocol, hostPort)
			m.mu.Lock()
			break
		}
	}

	ip, err := m.mgr.GetVMIP(ctx, vmId)
	if err != nil {
		return err
	}
	targetAddr := fmt.Sprintf("%s:%d", ip, guestPort)

	runCtx, cancel := context.WithCancel(context.Background())
	entry := &Entry{
		VMID:        vmId,
		Protocol:    protocol,
		HostPort:    hostPort,
		GuestPort:   guestPort,
		Mbps:        mbps,
		Description: description,
		cancel:      cancel,
	}

	limiter := NewRateLimiter(int64(mbps) * 1024 * 1024 / 8)

	if protocol == "tcp" {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", hostPort))
		if err != nil {
			cancel()
			return err
		}
		entry.ln = ln
		go m.runTCP(runCtx, ln, targetAddr, limiter)
	} else {
		pc, err := net.ListenPacket("udp", fmt.Sprintf(":%d", hostPort))
		if err != nil {
			cancel()
			return err
		}
		entry.ln = pc
		go m.runUDP(runCtx, pc, targetAddr, limiter)
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

func (m *Manager) RemoveMapping(ctx context.Context, vmId string, protocol string, hostPort int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	entries := m.mappings[vmId]
	for i, e := range entries {
		if e.Protocol == protocol && e.HostPort == hostPort {
			e.cancel()
			if e.ln != nil {
				e.ln.Close()
			}
			m.mappings[vmId] = append(entries[:i], entries[i+1:]...)
			_ = m.db.DeletePortForward(protocol, hostPort)
			return nil
		}
	}
	return nil
}

// SyncForVM 以 desired 为准全量对齐某个 VM 的端口转发规则
func (m *Manager) SyncForVM(ctx context.Context, vmId string, desired []DesiredRule, defaultMbps int) error {
	m.mu.RLock()
	current := m.mappings[vmId]
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
		_ = m.AddMapping(ctx, vmId, d.Protocol, d.HostPort, d.GuestPort, defaultMbps, d.Description)
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

// UpdateVMBandwidth 带宽变更时重建该 VM 所有转发规则并应用新限速
func (m *Manager) UpdateVMBandwidth(ctx context.Context, vmId string, mbps int) {
	m.mu.RLock()
	entries := make([]*Entry, len(m.mappings[vmId]))
	copy(entries, m.mappings[vmId])
	m.mu.RUnlock()

	for _, e := range entries {
		_ = m.RemoveMapping(ctx, vmId, e.Protocol, e.HostPort)
		_ = m.AddMapping(ctx, vmId, e.Protocol, e.HostPort, e.GuestPort, mbps, e.Description)
	}
}

func (m *Manager) GetReport() []Entry {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var all []Entry
	for _, entries := range m.mappings {
		for _, e := range entries {
			all = append(all, Entry{
				VMID:      e.VMID,
				Protocol:  e.Protocol,
				HostPort:  e.HostPort,
				GuestPort: e.GuestPort,
				Mbps:      e.Mbps,
			})
		}
	}
	return all
}

// --- 内部转发逻辑 ---

func (m *Manager) runTCP(ctx context.Context, ln net.Listener, target string, limiter *RateLimiter) {
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
		go m.handleTCP(ctx, conn, target, limiter)
	}
}

func (m *Manager) handleTCP(ctx context.Context, src net.Conn, target string, limiter *RateLimiter) {
	defer func() { _ = src.Close() }()
	dst, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		return
	}
	defer func() { _ = dst.Close() }()

	done := make(chan struct{}, 2)
	go func() {
		m.proxy(dst, src, limiter)
		done <- struct{}{}
	}()
	go func() {
		m.proxy(src, dst, limiter)
		done <- struct{}{}
	}()

	select {
	case <-ctx.Done():
	case <-done:
	}
}

func (m *Manager) runUDP(ctx context.Context, pc net.PacketConn, target string, limiter *RateLimiter) {
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
			client, _ = net.Dial("udp", targetAddr.String())
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
					if limiter != nil {
						limiter.Wait(rn)
					}
					_, _ = pc.WriteTo(rBuf[:rn], a)
				}
			}(addr, client)
		}
		smu.Unlock()

		if limiter != nil {
			limiter.Wait(n)
		}
		_, _ = client.Write(buf[:n])
	}
}

func (m *Manager) proxy(dst io.Writer, src io.Reader, limiter *RateLimiter) {
	if limiter != nil {
		dst = &LimitedWriter{w: dst, limiter: limiter}
	}
	_, _ = io.Copy(dst, src)
}
