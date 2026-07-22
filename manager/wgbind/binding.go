package wgbind

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"runman-agent/db"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const (
	// dialTimeout 是拨向 VM 内网 IPv4 的超时。目标就在同一台母鸡的网桥上，
	// 正常情况下微秒级返回（端口没开会立刻 ECONNREFUSED），超时只是兜底。
	dialTimeout = 5 * time.Second
	// udpIdleTimeout 与现有端口转发保持一致：UDP 无连接，只能靠空闲超时回收会话。
	udpIdleTimeout = 60 * time.Second
	// vmIPTTL 是 VM 内网 IP 的缓存时长。取短值使 VM 重装/重启换 IP 后能自动跟上，
	// 又不至于每条连接都去 inspect 一次容器。
	vmIPTTL = 10 * time.Second
	// maxInFlightSYN 限制同时处于三次握手中的连接数，防止 SYN flood 打爆内存。
	maxInFlightSYN = 2048
)

// IPCount 记录单个来源 IP 的连接情况（来源 IP 是隧道对端看到的客户端地址）。
type IPCount struct {
	IP          string `json:"ip"`
	ActiveCount int64  `json:"active_count"`
	Count       int64  `json:"count"`
}

// binding 是一条运行中的 WG 绑定。
//
// 数据通路：对端 UDP 报文 → wireguard-go 解密 → netTUN.Write → gVisor 协议栈
// → tcp/udp Forwarder（任意端口都命中）→ 拨到 VM 内网 IPv4 的同一端口 → 双向拷贝。
// 发往隧道地址的 ICMP echo 由 gVisor 直接应答，不进 Forwarder，也不会打扰 VM。
type binding struct {
	mgr  *Manager
	cfg  db.WGBinding // 配置快照，启动后不再修改（改配置 = 重建 binding）
	norm *normalized

	tun *netTUN
	dev *device.Device

	// VM 内网 IPv4 缓存，避免每条连接都去问驱动。
	ipMu      sync.Mutex
	cachedIP  string
	ipExpires time.Time

	// 统计
	connActive int64 // atomic
	connTotal  int64 // atomic
	connIPMu   sync.Mutex
	connIPs    map[string]int64
	activeIPs  map[string]int64

	// 最近一次拨号失败原因，用于面板排障（比如 VM 没开机）。
	errMu    sync.Mutex
	lastErr  string
	lastErrT time.Time

	ctx    context.Context
	cancel context.CancelFunc
}

// startBinding 拉起一条绑定：建协议栈 → 建 WG 设备 → 注册全端口 Forwarder。
// 注意这里刻意不去解析 VM 的 IP：隧道的存活与 VM 是否开机解耦，
// VM 关机时隧道照样握手、照样应答 ping，只是 TCP/UDP 连不上而已。
func startBinding(parent context.Context, m *Manager, cfg db.WGBinding, norm *normalized) (*binding, error) {
	nt, err := newNetTUN(norm.addr, norm.mtu)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(parent)
	b := &binding{
		mgr:       m,
		cfg:       cfg,
		norm:      norm,
		tun:       nt,
		connIPs:   make(map[string]int64),
		activeIPs: make(map[string]int64),
		ctx:       ctx,
		cancel:    cancel,
	}

	// 全端口接管：把协议栈里 TCP/UDP 的分发函数换成 Forwarder，
	// 这样不需要为每个端口 Listen，任何目的端口的首包都会走到我们的 handler。
	tcpFwd := tcp.NewForwarder(nt.stack, 0, maxInFlightSYN, b.handleTCP)
	nt.stack.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpFwd.HandlePacket)
	udpFwd := udp.NewForwarder(nt.stack, b.handleUDP)
	nt.stack.SetTransportProtocolHandler(udp.ProtocolNumber, udpFwd.HandlePacket)

	logger := device.NewLogger(device.LogLevelError, fmt.Sprintf("[wg %s] ", shortID(cfg.ID)))
	b.dev = device.NewDevice(nt, conn.NewDefaultBind(), logger)

	uapi, err := uapiConfig(&cfg, norm)
	if err != nil {
		b.stop()
		return nil, err
	}
	if err := b.dev.IpcSet(uapi); err != nil {
		b.stop()
		return nil, fmt.Errorf("apply wg config: %w", err)
	}
	if err := b.dev.Up(); err != nil {
		b.stop()
		return nil, fmt.Errorf("bring device up: %w", err)
	}
	return b, nil
}

func (b *binding) stop() {
	b.cancel()
	if b.dev != nil {
		b.dev.Close()
	}
	if b.tun != nil {
		_ = b.tun.Close()
	}
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// --- 目标地址解析 ---

// targetIP 返回 VM 的内网 IPv4，带短 TTL 缓存。
// 无论隧道地址是 IPv4 还是 IPv6，转发目标一律是这个内网 IPv4——
// 相当于在用户态做了一次协议族转换，VM 里只需要监听 IPv4 即可。
func (b *binding) targetIP() (string, error) {
	b.ipMu.Lock()
	if b.cachedIP != "" && time.Now().Before(b.ipExpires) {
		ip := b.cachedIP
		b.ipMu.Unlock()
		return ip, nil
	}
	b.ipMu.Unlock()

	ctx, cancel := context.WithTimeout(b.ctx, dialTimeout)
	defer cancel()
	ip, err := b.mgr.vmMgr.GetVMLocalIP(ctx, b.cfg.VMID)
	if err != nil {
		return "", err
	}
	if ip == "" {
		return "", fmt.Errorf("VM %s has no IP address (is it running?)", b.cfg.VMID)
	}

	b.ipMu.Lock()
	b.cachedIP = ip
	b.ipExpires = time.Now().Add(vmIPTTL)
	b.ipMu.Unlock()
	return ip, nil
}

// invalidateIP 在拨号失败后丢弃缓存，让下一条连接重新解析（VM 可能刚换了 IP）。
func (b *binding) invalidateIP() {
	b.ipMu.Lock()
	b.cachedIP = ""
	b.ipMu.Unlock()
}

// dialVM 按 1:1 端口映射拨向 VM。
func (b *binding) dialVM(network string, port uint16) (net.Conn, error) {
	ip, err := b.targetIP()
	if err != nil {
		b.noteErr(err)
		return nil, err
	}
	target := net.JoinHostPort(ip, strconv.Itoa(int(port)))
	c, err := net.DialTimeout(network, target, dialTimeout)
	if err != nil {
		b.invalidateIP()
		b.noteErr(err)
		return nil, err
	}
	return c, nil
}

func (b *binding) noteErr(err error) {
	b.errMu.Lock()
	b.lastErr = err.Error()
	b.lastErrT = time.Now()
	b.errMu.Unlock()
}

// --- 转发 handler ---

// handleTCP 处理一条新的 TCP 连接请求。
// gVisor 的 tcp.Forwarder 已经为每个请求起了独立 goroutine，这里可以放心阻塞。
//
// 先拨 VM 再完成握手：这样 VM 上没监听的端口能如实回 RST，客户端立刻得到
// "connection refused"，而不是先握上手再被断开。
func (b *binding) handleTCP(r *tcp.ForwarderRequest) {
	id := r.ID()
	dst, err := b.dialVM("tcp", id.LocalPort)
	if err != nil {
		r.Complete(true) // 发 RST
		return
	}

	var wq waiter.Queue
	ep, tcpErr := r.CreateEndpoint(&wq)
	if tcpErr != nil {
		_ = dst.Close()
		r.Complete(true)
		return
	}
	r.Complete(false)

	// 转发场景下延迟比吞吐重要，关掉 Nagle。
	ep.SocketOptions().SetDelayOption(false)
	src := gonet.NewTCPConn(&wq, ep)

	remoteIP := addrString(id.RemoteAddress)
	b.trackConnect(remoteIP)
	defer b.trackDisconnect(remoteIP)

	defer func() { _ = src.Close() }()
	defer func() { _ = dst.Close() }()

	done := make(chan struct{}, 2)
	go func() { copyAndCloseWrite(dst, src); done <- struct{}{} }()
	go func() { copyAndCloseWrite(src, dst); done <- struct{}{} }()

	select {
	case <-b.ctx.Done():
	case <-done:
		// 一个方向结束后等另一个方向收尾，给半关闭的连接留出排空时间。
		select {
		case <-done:
		case <-b.ctx.Done():
		case <-time.After(30 * time.Second):
		}
	}
}

// copyAndCloseWrite 单向拷贝，源端 EOF 后半关闭目标的写方向，
// 让对端能正常感知到流结束（否则依赖 EOF 的协议会卡住）。
func copyAndCloseWrite(dst, src net.Conn) {
	_, _ = io.Copy(dst, src)
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// handleUDP 处理一个新的 UDP 会话（每个四元组一次）。
// CreateEndpoint 必须同步调用——它要用请求里携带的首包，返回后那个包就没了。
func (b *binding) handleUDP(r *udp.ForwarderRequest) {
	id := r.ID()
	var wq waiter.Queue
	ep, tcpErr := r.CreateEndpoint(&wq)
	if tcpErr != nil {
		return
	}
	src := gonet.NewUDPConn(&wq, ep)
	go b.pumpUDP(src, id)
}

func (b *binding) pumpUDP(src *gonet.UDPConn, id stack.TransportEndpointID) {
	defer func() { _ = src.Close() }()

	dst, err := b.dialVM("udp", id.LocalPort)
	if err != nil {
		return
	}
	defer func() { _ = dst.Close() }()

	remoteIP := addrString(id.RemoteAddress)
	b.trackConnect(remoteIP)
	defer b.trackDisconnect(remoteIP)

	done := make(chan struct{}, 2)
	pump := func(w, r net.Conn) {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 64*1024)
		for {
			_ = r.SetReadDeadline(time.Now().Add(udpIdleTimeout))
			n, err := r.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	go pump(dst, src)
	go pump(src, dst)

	select {
	case <-b.ctx.Done():
	case <-done:
	}
}

// addrString 把 gVisor 的地址转成可读字符串（IPv4 点分十进制 / IPv6 冒号形式）。
func addrString(a tcpip.Address) string {
	if ip, ok := netip.AddrFromSlice(a.AsSlice()); ok {
		return ip.Unmap().String()
	}
	return a.String()
}

// --- 统计 ---

func (b *binding) trackConnect(ip string) {
	atomic.AddInt64(&b.connActive, 1)
	atomic.AddInt64(&b.connTotal, 1)
	b.connIPMu.Lock()
	b.connIPs[ip]++
	b.activeIPs[ip]++
	b.connIPMu.Unlock()
}

func (b *binding) trackDisconnect(ip string) {
	atomic.AddInt64(&b.connActive, -1)
	b.connIPMu.Lock()
	if b.activeIPs[ip] > 1 {
		b.activeIPs[ip]--
	} else {
		delete(b.activeIPs, ip)
	}
	b.connIPMu.Unlock()
}

func (b *binding) topIPs(limit int) []IPCount {
	b.connIPMu.Lock()
	out := make([]IPCount, 0, len(b.connIPs))
	for ip, c := range b.connIPs {
		out = append(out, IPCount{IP: ip, Count: c, ActiveCount: b.activeIPs[ip]})
	}
	b.connIPMu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].ActiveCount != out[j].ActiveCount {
			return out[i].ActiveCount > out[j].ActiveCount
		}
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].IP < out[j].IP
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// peerStats 从 wireguard-go 读取隧道自身的状态。
// IpcGet 的输出里含私钥，这里只挑需要的字段，绝不整串外泄。
type peerStats struct {
	ListenPort    int
	Endpoint      string
	LastHandshake time.Time
	RxBytes       int64
	TxBytes       int64
}

func (b *binding) peerStats() peerStats {
	var ps peerStats
	raw, err := b.dev.IpcGet()
	if err != nil {
		return ps
	}
	var hsSec int64
	for _, line := range strings.Split(raw, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "listen_port":
			ps.ListenPort, _ = strconv.Atoi(v)
		case "endpoint":
			ps.Endpoint = v
		case "last_handshake_time_sec":
			hsSec, _ = strconv.ParseInt(v, 10, 64)
		case "rx_bytes":
			ps.RxBytes, _ = strconv.ParseInt(v, 10, 64)
		case "tx_bytes":
			ps.TxBytes, _ = strconv.ParseInt(v, 10, 64)
		}
	}
	if hsSec > 0 {
		ps.LastHandshake = time.Unix(hsSec, 0)
	}
	return ps
}
