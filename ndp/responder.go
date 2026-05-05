//go:build linux

package ndp

import (
	"context"
	"log"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/afpacket"
	"go4.org/netipx"
)

// Responder answers ICMPv6 neighbor solicitations on behalf of container and VM IPs.
//
// It responds to solicitations for:
//   - Any address that falls inside a configured static subnet (Subnets)
//   - Any IPv6 address currently assigned to a container in the watched Podman network (PodmanNetwork)
//   - Any IPv6 address assigned to a cloud-hypervisor VM (CloudHVDB)
type Responder struct {
	iface      string
	subnets    *netipx.IPSet
	podmanNet  string
	podmanSock string
	cloudhvDB  interface{} // *db.DB (avoid circular import)
	incusDB    interface{} // *db.DB
}

// New creates a Responder.
//
//   - iface:       uplink interface name (e.g. "eth0")
//   - subnets:     comma-separated list of IPv6 CIDRs to respond for (may be empty)
//   - podmanNet:   Podman network name to watch for container IPs (may be empty)
//   - podmanSock:  path to Podman API socket (e.g. "unix:///run/podman/podman.sock")
//   - cloudhvDB:   database for cloud-hypervisor VMs (*db.DB, may be nil)
//   - incusDB:     database for incus instances (*db.DB, may be nil)
func New(iface string, subnets string, podmanNet string, podmanSock string, cloudhvDB interface{}, incusDB interface{}) (*Responder, error) {
	var b netipx.IPSetBuilder
	for _, s := range strings.Split(subnets, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, err
		}
		b.AddPrefix(prefix)
	}
	ipset, err := b.IPSet()
	if err != nil {
		return nil, err
	}
	return &Responder{
		iface:      iface,
		subnets:    ipset,
		podmanNet:  podmanNet,
		podmanSock: podmanSock,
		cloudhvDB:  cloudhvDB,
		incusDB:    incusDB,
	}, nil
}

// Run starts the NDP responder. It blocks until ctx is cancelled or a fatal error occurs.
func (r *Responder) Run(ctx context.Context) error {
	netIface, err := net.InterfaceByName(r.iface)
	if err != nil {
		return err
	}

	hi, err := gatherHostInfo(netIface)
	if err != nil {
		return err
	}

	h, err := afpacket.NewTPacket(afpacket.OptInterface(r.iface))
	if err != nil {
		return err
	}

	if err := h.SetBPF(bpfFilter); err != nil {
		h.Close()
		return err
	}

	// Close the handle when ctx is done so packet reads unblock.
	go func() {
		<-ctx.Done()
		h.Close()
	}()

	solicitations := CaptureNeighSolicitation(h)

	var podmanTracker *podmanTracker
	var cloudhvTracker *cloudhvTracker
	var incusTracker *incusTracker

	// Combine newIPs from all active trackers
	newIPsCh := make(chan netip.Addr, 128)

	if r.podmanNet != "" {
		podmanTracker = newPodmanTracker(r.podmanSock, r.podmanNet)
		go podmanTracker.run(ctx)
		go func() {
			for ip := range podmanTracker.newIPs {
				newIPsCh <- ip
			}
		}()
	}

	if r.cloudhvDB != nil {
		cloudhvTracker = newCloudHVTracker(r.cloudhvDB)
		if cloudhvTracker != nil {
			go cloudhvTracker.run(ctx)
			go func() {
				for ip := range cloudhvTracker.newIPs {
					newIPsCh <- ip
				}
			}()
		}
	}

	if r.incusDB != nil {
		incusTracker = newIncusTracker(r.incusDB)
		if incusTracker != nil {
			go incusTracker.run(ctx)
			go func() {
				for ip := range incusTracker.newIPs {
					newIPsCh <- ip
				}
			}()
		}
	}

	log.Printf("NDP responder started on %s (podman: %v, cloudhv: %v, incus: %v)", r.iface, podmanTracker != nil, cloudhvTracker != nil, incusTracker != nil)
	sbuf := gopacket.NewSerializeBuffer()

	// 周期刷新：每 10 分钟对所有活跃容器 IP 发 gratuitous NDP，
	// 防止上游路由器/交换机的 NDP 缓存老化后入站中断。
	ndpRefreshTicker := time.NewTicker(10 * time.Minute)
	defer ndpRefreshTicker.Stop()

	sendGratuitous := func(ip netip.Addr) {
		if err := Gratuitous(sbuf, hi, ip); err == nil {
			h.WritePacketData(sbuf.Bytes())
		}
		if hi.GatewayIP.IsValid() {
			if err := Solicit(sbuf, hi, ip); err == nil {
				h.WritePacketData(sbuf.Bytes())
			}
		}
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case ns, ok := <-solicitations:
			if !ok {
				return nil
			}
			var respond bool
			if podmanTracker != nil && podmanTracker.contains(ns.TargetIP) {
				respond = true
			} else if cloudhvTracker != nil && cloudhvTracker.contains(ns.TargetIP) {
				respond = true
			} else if incusTracker != nil && incusTracker.contains(ns.TargetIP) {
				respond = true
			} else if ns.DestIP.IsMulticast() && r.subnets.Contains(ns.TargetIP) {
				respond = true
			}
			if !respond {
				continue
			}
			if err := ns.Respond(sbuf, hi); err != nil {
				log.Printf("NDP respond error: %v", err)
				continue
			}
			h.WritePacketData(sbuf.Bytes())
			log.Printf("NDP: responded for %s (solicited by %s)", ns.TargetIP, ns.RouterIP)

		case ip, ok := <-newIPsCh:
			if !ok {
				continue
			}
			sendGratuitous(ip)
			log.Printf("NDP: sent gratuitous/solicit for new container IP %s", ip)

		case <-ndpRefreshTicker.C:
			var allIPs []netip.Addr
			if podmanTracker != nil {
				allIPs = append(allIPs, podmanTracker.allIPs()...)
			}
			if cloudhvTracker != nil {
				allIPs = append(allIPs, cloudhvTracker.allIPs()...)
			}
			if incusTracker != nil {
				allIPs = append(allIPs, incusTracker.allIPs()...)
			}
			for _, ip := range allIPs {
				sendGratuitous(ip)
			}
			if len(allIPs) > 0 {
				log.Printf("NDP: periodic refresh sent for %d IPs", len(allIPs))
			}
		}
	}
}
