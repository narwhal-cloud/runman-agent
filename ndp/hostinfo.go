//go:build linux

package ndp

import (
	"log"
	"net"
	"net/netip"
	"os/exec"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// gatherHostInfo resolves the MAC address and default IPv6 gateway for iface.
func gatherHostInfo(iface *net.Interface) (hi HostInfo, e error) {
	hi.HostMAC = iface.HardwareAddr
	log.Printf("NDP: interface %s MAC=%s", iface.Name, hi.HostMAC)

	nl, e := netlink.NewHandle()
	if e != nil {
		log.Printf("NDP: netlink.NewHandle: %v", e)
		return hi, nil
	}
	defer nl.Close()

	link, e := nl.LinkByIndex(iface.Index)
	if e != nil {
		log.Printf("NDP: netlink.LinkByIndex: %v", e)
		return hi, nil
	}

	routes, e := nl.RouteList(link, unix.AF_INET6)
	if e != nil {
		log.Printf("NDP: netlink.RouteList: %v", e)
		return hi, nil
	}

	for _, route := range routes {
		maskLen := 0
		if route.Dst != nil {
			maskLen, _ = route.Dst.Mask.Size()
		}
		if maskLen == 0 {
			hi.GatewayIP, _ = netip.AddrFromSlice(route.Gw)
			hi.GatewayIP = hi.GatewayIP.Unmap()
		}
	}
	if !hi.GatewayIP.IsValid() {
		log.Printf("NDP: no default IPv6 gateway on %s", iface.Name)
		return hi, nil
	}
	log.Printf("NDP: gateway %s", hi.GatewayIP)

	// Wait until the gateway neighbour entry is resolved so we can pin it as NUD_NOARP.
	var gatewayNeigh *netlink.Neigh
	for {
		neighs, e := nl.NeighList(iface.Index, unix.AF_INET6)
		if e != nil {
			log.Printf("NDP: netlink.NeighList: %v", e)
			return hi, nil
		}
		for _, neigh := range neighs {
			ip, _ := netip.AddrFromSlice(neigh.IP)
			ip = ip.Unmap()
			if ip != hi.GatewayIP || len(neigh.HardwareAddr) != 6 {
				continue
			}
			switch neigh.State {
			case unix.NUD_REACHABLE, unix.NUD_NOARP:
				gatewayNeigh = &neigh
				goto NEIGH_SET
			case unix.NUD_PERMANENT:
				goto NEIGH_SKIP
			}
		}
		exec.Command("/usr/bin/ping", "-c", "1", hi.GatewayIP.String()).Run()
		time.Sleep(time.Second)
	}

NEIGH_SET:
	gatewayNeigh.State = unix.NUD_NOARP
	if e = nl.NeighSet(gatewayNeigh); e != nil {
		log.Printf("NDP: netlink.NeighSet: %v", e)
	} else {
		log.Printf("NDP: pinned gateway neigh %s", gatewayNeigh.HardwareAddr)
	}
NEIGH_SKIP:
	return hi, nil
}
