//go:build linux

package ndp

import (
	"context"
	"log"
	"net"
	"net/netip"
	"strings"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/afpacket"
	"go4.org/netipx"
)

// Responder answers ICMPv6 neighbor solicitations on behalf of container IPs.
//
// It responds to solicitations for:
//   - Any address that falls inside a configured static subnet (Subnets)
//   - Any IPv6 address currently assigned to a container in the watched Podman network (PodmanNetwork)
type Responder struct {
	iface      string
	subnets    *netipx.IPSet
	podmanNet  string
	podmanSock string
}

// New creates a Responder.
//
//   - iface:       uplink interface name (e.g. "eth0")
//   - subnets:     comma-separated list of IPv6 CIDRs to respond for (may be empty)
//   - podmanNet:   Podman network name to watch for container IPs (may be empty)
//   - podmanSock:  path to Podman API socket (e.g. "unix:///run/podman/podman.sock")
func New(iface string, subnets string, podmanNet string, podmanSock string) (*Responder, error) {
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

	var tracker *podmanTracker
	var newIPsCh <-chan netip.Addr
	if r.podmanNet != "" {
		tracker = newPodmanTracker(r.podmanSock, r.podmanNet)
		go tracker.run(ctx)
		newIPsCh = tracker.newIPs
	} else {
		newIPsCh = make(chan netip.Addr) // never sends
	}

	log.Printf("NDP responder started on %s", r.iface)
	sbuf := gopacket.NewSerializeBuffer()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case ns, ok := <-solicitations:
			if !ok {
				return nil
			}
			var respond bool
			if tracker != nil && tracker.contains(ns.TargetIP) {
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
			if err := Gratuitous(sbuf, hi, ip); err == nil {
				h.WritePacketData(sbuf.Bytes())
			}
			if hi.GatewayIP.IsValid() {
				if err := Solicit(sbuf, hi, ip); err == nil {
					h.WritePacketData(sbuf.Bytes())
				}
			}
			log.Printf("NDP: sent gratuitous/solicit for new container IP %s", ip)
		}
	}
}
