package wgbind

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"runman-agent/db"
	"strconv"
	"strings"

	"golang.org/x/crypto/curve25519"
)

const (
	// DefaultMTU 取 WireGuard 的常规值：1500 - 20(IPv4) - 8(UDP) - 32(WG 头) = 1440，
	// 保守留到 1420 以兼容 PPPoE / 外层再套一层隧道的链路。
	DefaultMTU = 1420
	// MinMTU 低于这个值 TCP 基本没法用，直接判为配置错误。
	MinMTU = 576
	MaxMTU = 9000

	// DefaultKeepalive 是配了 Endpoint（我们是主动方）时的默认 keepalive 秒数。
	//
	// 这个默认值不是可选的优化，而是隧道能不能通的前提：本绑定只做入站转发，
	// netstack 自己永远不产生出站流量，而 wireguard-go 仅在 persistent keepalive
	// 大于 0 时才会在 Up() 里主动发起握手（见 device.upLocked）。keepalive 为 0
	// 时设备会一直干等对端来连，但我们的 UDP 端口通常是随机的，对端根本找不到我们，
	// 结果就是永远 "handshake never"。25s 也是 wg-quick 的常规值，顺带保住 NAT 映射。
	DefaultKeepalive = 25
)

// defaultAllowedIPs 表示"所有流量都可能从这个对端来/回这个对端去"。
// 我们只做入站转发，回包必须能路由回对端，所以默认全放开。
var defaultAllowedIPs = []string{"0.0.0.0/0", "::/0"}

// parseKey 校验并解码一个 base64 编码的 32 字节 WireGuard 密钥。
func parseKey(s string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("not valid base64: %w", err)
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("expected 32 bytes, got %d", len(raw))
	}
	return raw, nil
}

// PublicKeyOf 由 base64 私钥推导出 base64 公钥，供面板展示"本端公钥"
// （用户需要把它填到对端的 [Peer] 里）。
func PublicKeyOf(privateKey string) (string, error) {
	priv, err := parseKey(privateKey)
	if err != nil {
		return "", err
	}
	pub, err := curve25519.X25519(priv, curve25519.Basepoint)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(pub), nil
}

// GenerateKeyPair 生成一对 Curve25519 密钥（base64），供面板"生成密钥"按钮使用。
func GenerateKeyPair() (privateKey, publicKey string, err error) {
	var priv [32]byte
	if _, err = rand.Read(priv[:]); err != nil {
		return "", "", err
	}
	// WireGuard 的私钥 clamping，保证与 wg genkey 产出的格式一致。
	priv[0] &= 248
	priv[31] &= 127
	priv[31] |= 64

	pub, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(priv[:]),
		base64.StdEncoding.EncodeToString(pub), nil
}

// normalized 是校验通过后的绑定配置，字段都已规整成可直接使用的形式。
type normalized struct {
	addr       netip.Addr
	mtu        int
	listenPort int
	allowedIPs []string
}

// validate 校验并规整一条绑定配置。b 会被就地补全默认值（MTU、AllowedIPs 等），
// 使得存进 DB 的记录和实际生效的配置一致，面板上看到什么就是什么。
func validate(b *db.WGBinding) (*normalized, error) {
	n := &normalized{}

	if _, err := parseKey(b.PrivateKey); err != nil {
		return nil, fmt.Errorf("private_key: %w", err)
	}
	if _, err := parseKey(b.PeerPublicKey); err != nil {
		return nil, fmt.Errorf("peer_public_key: %w", err)
	}
	if b.PresharedKey != "" {
		if _, err := parseKey(b.PresharedKey); err != nil {
			return nil, fmt.Errorf("preshared_key: %w", err)
		}
	}

	// 隧道地址：允许带前缀长度（10.7.0.5/32），但只取地址部分——
	// 每个绑定只认一个地址，前缀长度对纯用户态协议栈没有意义。
	addrStr := strings.TrimSpace(b.Address)
	if addrStr == "" {
		return nil, fmt.Errorf("address is required")
	}
	if i := strings.IndexByte(addrStr, '/'); i >= 0 {
		addrStr = addrStr[:i]
	}
	addr, err := netip.ParseAddr(addrStr)
	if err != nil {
		return nil, fmt.Errorf("address: %w", err)
	}
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	if !addr.IsValid() || addr.IsUnspecified() {
		return nil, fmt.Errorf("address: %s is not a usable tunnel address", addrStr)
	}
	n.addr = addr
	b.Address = addr.String()

	if b.MTU == 0 {
		b.MTU = DefaultMTU
	}
	if b.MTU < MinMTU || b.MTU > MaxMTU {
		return nil, fmt.Errorf("mtu %d out of range [%d, %d]", b.MTU, MinMTU, MaxMTU)
	}
	n.mtu = b.MTU

	if b.ListenPort < 0 || b.ListenPort > 65535 {
		return nil, fmt.Errorf("listen_port %d out of range", b.ListenPort)
	}
	n.listenPort = b.ListenPort

	// Endpoint 可以为空：那样 Agent 就是被动方，等对端连进来（此时应指定 ListenPort，
	// 否则每次重启端口都变，对端找不到我们）。
	if ep := strings.TrimSpace(b.Endpoint); ep != "" {
		host, portStr, err := net.SplitHostPort(ep)
		if err != nil {
			return nil, fmt.Errorf("endpoint: expected host:port, got %q", ep)
		}
		port, err := strconv.Atoi(portStr)
		if err != nil || port <= 0 || port > 65535 {
			return nil, fmt.Errorf("endpoint: invalid port %q", portStr)
		}
		if host == "" {
			return nil, fmt.Errorf("endpoint: host is empty")
		}
		b.Endpoint = ep
		// 主动方必须有 keepalive，否则永远不会发起握手，见 DefaultKeepalive 的说明。
		if b.Keepalive == 0 {
			b.Keepalive = DefaultKeepalive
		}
	} else {
		b.Endpoint = ""
		if b.ListenPort == 0 {
			return nil, fmt.Errorf("listen_port is required when endpoint is empty (peer needs a fixed port to reach us)")
		}
	}

	if b.Keepalive < 0 || b.Keepalive > 65535 {
		return nil, fmt.Errorf("keepalive %d out of range", b.Keepalive)
	}

	n.allowedIPs, err = parseAllowedIPs(b.AllowedIPs)
	if err != nil {
		return nil, err
	}
	b.AllowedIPs = strings.Join(n.allowedIPs, ", ")

	return n, nil
}

func parseAllowedIPs(s string) ([]string, error) {
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' })
	if len(fields) == 0 {
		return append([]string(nil), defaultAllowedIPs...), nil
	}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		p, err := netip.ParsePrefix(f)
		if err != nil {
			// 允许写裸地址，按 /32、/128 处理
			addr, aerr := netip.ParseAddr(f)
			if aerr != nil {
				return nil, fmt.Errorf("allowed_ips: %q is not a valid CIDR or address", f)
			}
			p = netip.PrefixFrom(addr, addr.BitLen())
		}
		out = append(out, p.Masked().String())
	}
	return out, nil
}

// uapiConfig 把绑定配置渲染成 wireguard-go 的 IPC 配置串。
// 注意 UAPI 里所有密钥都是十六进制，而配置文件/面板用的是 base64，这里做转换。
func uapiConfig(b *db.WGBinding, n *normalized) (string, error) {
	hexKey := func(s string) (string, error) {
		raw, err := parseKey(s)
		if err != nil {
			return "", err
		}
		return hex.EncodeToString(raw), nil
	}

	priv, err := hexKey(b.PrivateKey)
	if err != nil {
		return "", err
	}
	peer, err := hexKey(b.PeerPublicKey)
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "private_key=%s\n", priv)
	fmt.Fprintf(&sb, "listen_port=%d\n", n.listenPort)
	sb.WriteString("replace_peers=true\n")
	fmt.Fprintf(&sb, "public_key=%s\n", peer)
	if b.PresharedKey != "" {
		psk, err := hexKey(b.PresharedKey)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&sb, "preshared_key=%s\n", psk)
	}
	if b.Endpoint != "" {
		fmt.Fprintf(&sb, "endpoint=%s\n", b.Endpoint)
	}
	if b.Keepalive > 0 {
		fmt.Fprintf(&sb, "persistent_keepalive_interval=%d\n", b.Keepalive)
	}
	sb.WriteString("replace_allowed_ips=true\n")
	for _, ip := range n.allowedIPs {
		fmt.Fprintf(&sb, "allowed_ip=%s\n", ip)
	}
	return sb.String(), nil
}
