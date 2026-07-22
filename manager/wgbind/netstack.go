// netstack.go 提供一块纯用户态的虚拟网卡 + TCP/IP 协议栈：对上给 wireguard-go 当
// tun.Device 用，对下把解密后的 IP 包送进 gVisor 的 netstack。整条链路不碰内核
// TUN、不碰 netfilter，因此不需要 CAP_NET_ADMIN，也不会在宿主机上留下网络配置。
//
// 为什么不直接用 wireguard-go 自带的 tun/netstack 包：官方包只导出 Dial/Listen 这类
// 按端口精确匹配的高层 API，做不到"任意端口都接管"。我们要的是把整个隧道地址透传给
// VM，必须自己拿到底层 *stack.Stack 去注册 tcp.Forwarder / udp.Forwarder，所以这里
// 照着它的思路重写一份，把 stack 暴露出来。
package wgbind

import (
	"fmt"
	"net/netip"
	"os"
	"sync"

	"golang.zx2c4.com/wireguard/tun"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

const nicID tcpip.NICID = 1

// netTUN 实现 wireguard-go 的 tun.Device 接口。
// wireguard-go 把解密后的明文 IP 包通过 Write 交给我们 → 注入 gVisor；
// gVisor 要发出的包通过 WriteNotify 回调进 incoming 队列 → wireguard-go 从 Read 取走加密外发。
type netTUN struct {
	ep       *channel.Endpoint
	stack    *stack.Stack
	events   chan tun.Event
	incoming chan *buffer.View
	mtu      int

	// closeOnce/closed 保证 Close 幂等，且 WriteNotify 在关闭后不会往已关的
	// channel 上发送（gVisor 的 endpoint 可能与 Close 并发回调）。
	closeOnce sync.Once
	closed    chan struct{}
}

// newNetTUN 创建虚拟网卡 + 协议栈，并把 localAddr 绑定到网卡上。
// 绑定地址后 gVisor 会自动应答发往该地址的 ICMP echo（ping），无需我们插手，
// 也不依赖 raw socket——这正是"Agent 本地应答 ping"的实现方式。
// 返回的 *stack.Stack 供调用方注册 TCP/UDP Forwarder 做全端口转发。
func newNetTUN(localAddr netip.Addr, mtu int) (*netTUN, error) {
	protoNumber := ipv4.ProtocolNumber
	if localAddr.Is6() {
		protoNumber = ipv6.ProtocolNumber
	}

	t := &netTUN{
		ep:       channel.New(512, uint32(mtu), ""),
		events:   make(chan tun.Event, 4),
		incoming: make(chan *buffer.View, 512),
		mtu:      mtu,
		closed:   make(chan struct{}),
	}
	t.stack = stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4, icmp.NewProtocol6},
		HandleLocal:        true,
	})

	sackEnabled := tcpip.TCPSACKEnabled(true)
	if err := t.stack.SetTransportProtocolOption(tcp.ProtocolNumber, &sackEnabled); err != nil {
		return nil, fmt.Errorf("enable tcp sack: %v", err)
	}

	t.ep.AddNotify(t)
	if err := t.stack.CreateNIC(nicID, t.ep); err != nil {
		return nil, fmt.Errorf("create nic: %v", err)
	}

	protoAddr := tcpip.ProtocolAddress{
		Protocol:          protoNumber,
		AddressWithPrefix: tcpip.AddrFromSlice(localAddr.AsSlice()).WithPrefix(),
	}
	if err := t.stack.AddProtocolAddress(nicID, protoAddr, stack.AddressProperties{}); err != nil {
		return nil, fmt.Errorf("add address %s: %v", localAddr, err)
	}

	// 默认路由：回包一律从这块网卡出去（也就是回到 WG 隧道对端）。
	// 两个协议族都加，隧道地址是 v4 还是 v6 都不用改代码。
	t.stack.AddRoute(tcpip.Route{Destination: header.IPv4EmptySubnet, NIC: nicID})
	t.stack.AddRoute(tcpip.Route{Destination: header.IPv6EmptySubnet, NIC: nicID})

	t.events <- tun.EventUp
	return t, nil
}

func (t *netTUN) Name() (string, error)    { return "wgbind", nil }
func (t *netTUN) File() *os.File           { return nil }
func (t *netTUN) Events() <-chan tun.Event { return t.events }
func (t *netTUN) MTU() (int, error)        { return t.mtu, nil }
func (t *netTUN) BatchSize() int           { return 1 }

// Read 供 wireguard-go 取出待加密外发的包。
func (t *netTUN) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	select {
	case view, ok := <-t.incoming:
		if !ok {
			return 0, os.ErrClosed
		}
		n, err := view.Read(bufs[0][offset:])
		if err != nil {
			return 0, err
		}
		sizes[0] = n
		return 1, nil
	case <-t.closed:
		return 0, os.ErrClosed
	}
}

// Write 收下 wireguard-go 解密后的明文 IP 包，注入协议栈。
func (t *netTUN) Write(bufs [][]byte, offset int) (int, error) {
	select {
	case <-t.closed:
		return 0, os.ErrClosed
	default:
	}
	for _, buf := range bufs {
		packet := buf[offset:]
		if len(packet) == 0 {
			continue
		}
		pkb := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(packet)})
		switch packet[0] >> 4 {
		case 4:
			t.ep.InjectInbound(header.IPv4ProtocolNumber, pkb)
		case 6:
			t.ep.InjectInbound(header.IPv6ProtocolNumber, pkb)
		}
		pkb.DecRef()
	}
	return len(bufs), nil
}

// WriteNotify 由 channel.Endpoint 在协议栈有包要发出时回调。
// incoming 有缓冲，满了就丢包——IP 层本就允许丢包，阻塞在这里反而会卡住整个协议栈。
func (t *netTUN) WriteNotify() {
	pkt := t.ep.Read()
	if pkt == nil {
		return
	}
	view := pkt.ToView()
	pkt.DecRef()

	select {
	case t.incoming <- view:
	case <-t.closed:
	default: // 队列满，丢弃
	}
}

func (t *netTUN) Close() error {
	t.closeOnce.Do(func() {
		close(t.closed)
		t.stack.RemoveNIC(nicID)
		t.ep.Close()
		close(t.events)
	})
	return nil
}
