//go:build linux

package ndp

import (
	"context"
	"log"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/containers/podman/v5/pkg/bindings"
	"github.com/containers/podman/v5/pkg/bindings/network"
	"go4.org/netipx"
)

// podmanTracker polls a Podman network for container IPv6 addresses and exposes them
// via Contains() and a newIPs channel so the NDP responder can react immediately.
type podmanTracker struct {
	socketPath string
	network    string

	mu        sync.RWMutex
	activeIPs *netipx.IPSet
	ctSeen    map[string]struct{}

	newIPs chan netip.Addr
}

func newPodmanTracker(socketPath, networkName string) *podmanTracker {
	return &podmanTracker{
		socketPath: socketPath,
		network:    networkName,
		activeIPs:  &netipx.IPSet{},
		ctSeen:     make(map[string]struct{}),
		newIPs:     make(chan netip.Addr, 64),
	}
}

func (t *podmanTracker) contains(ip netip.Addr) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.activeIPs.Contains(ip)
}

func (t *podmanTracker) allIPs() []netip.Addr {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var ips []netip.Addr
	for _, p := range t.activeIPs.Prefixes() {
		if p.IsSingleIP() {
			ips = append(ips, p.Addr())
		}
	}
	return ips
}

func (t *podmanTracker) run(ctx context.Context) {
	client, err := bindings.NewConnection(ctx, t.socketPath)
	if err != nil {
		log.Printf("NDP podman tracker: connect %s: %v", t.socketPath, err)
		return
	}
	t.refresh(client)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.refresh(client)
		}
	}
}

func (t *podmanTracker) refresh(client context.Context) {
	inspect, err := network.Inspect(client, t.network, nil)
	if err != nil {
		log.Printf("NDP podman tracker: inspect network %s: %v", t.network, err)
		return
	}

	var b netipx.IPSetBuilder
	var newIPs []netip.Addr

	for ctID, ct := range inspect.Containers {
		for _, iface := range ct.Interfaces {
			for _, subnet := range iface.Subnets {
				raw := subnet.IPNet.IP
				if !raw.IsGlobalUnicast() || len(raw) != net.IPv6len {
					continue
				}
				ip := netip.AddrFrom16([16]byte(raw.To16())).Unmap()
				if !ip.Is6() {
					continue
				}
				b.Add(ip)
				if _, seen := t.ctSeen[ctID]; !seen {
					newIPs = append(newIPs, ip)
					t.ctSeen[ctID] = struct{}{}
				}
			}
		}
	}

	ipset, _ := b.IPSet()
	t.mu.Lock()
	t.activeIPs = ipset
	t.mu.Unlock()

	for _, ip := range newIPs {
		select {
		case t.newIPs <- ip:
		default:
		}
	}
}
