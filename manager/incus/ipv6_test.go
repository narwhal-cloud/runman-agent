//go:build linux

package incus

import (
	"context"
	"strings"
	"testing"
)

func TestCustomAlpineIsDefaultImage(t *testing.T) {
	m := &Manager{}
	images, err := m.GetSupportedImages(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(images) < 2 || images[0].Id != "alpine" || images[1].Id != "debian" {
		t.Fatalf("unexpected Incus image order: %#v", images)
	}
}

func TestComputeIPsCanonicalCIDRs(t *testing.T) {
	tests := []struct {
		name   string
		cidr   string
		alloc  int
		want   []string
		gwWant string
	}{
		{
			name:   "compressed routed slash64",
			cidr:   "2001:470:36:154::/64",
			alloc:  1,
			want:   []string{"2001:470:36:154::2"},
			gwWant: "2001:470:36:154::ffff",
		},
		{
			name:   "ten addresses",
			cidr:   "2001:db8:1:2::/64",
			alloc:  10,
			want:   []string{"2001:db8:1:2::2", "2001:db8:1:2::b"},
			gwWant: "2001:db8:1:2::ffff",
		},
		{
			name:   "non slash64",
			cidr:   "2001:db8:abcd:1200::/80",
			alloc:  1,
			want:   []string{"2001:db8:abcd:1200::2"},
			gwWant: "2001:db8:abcd:1200::ffff",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manager{ipv6Mode: "subnet", ipv6Subnet: tc.cidr, ipv6Alloc: tc.alloc}
			_, got := m.computeIPs(2)
			if len(got) != tc.alloc {
				t.Fatalf("got %d addresses, want %d: %v", len(got), tc.alloc, got)
			}
			if got[0] != tc.want[0] || got[len(got)-1] != tc.want[len(tc.want)-1] {
				t.Fatalf("got %v, want first/last %v", got, tc.want)
			}
			if gw := m.ipv6Gateway(); gw != tc.gwWant {
				t.Fatalf("gateway %q, want %q", gw, tc.gwWant)
			}
		})
	}
}

func TestSNATUsesBridgeGateway(t *testing.T) {
	m := &Manager{ipv6Mode: "snat", ipv6Addr: "2001:db8::10", ipv6Alloc: 1}
	_, addresses := m.computeIPs(2)
	if len(addresses) != 1 || addresses[0] != "fd91:cafe:cafe:10::2" {
		t.Fatalf("unexpected SNAT address allocation: %v", addresses)
	}
	if got := m.ipv6Gateway(); got != "fd91:cafe:cafe:10::1" {
		t.Fatalf("SNAT gateway %q, want bridge gateway", got)
	}
}

func TestInstanceResolvConfIPv6Only(t *testing.T) {
	got := instanceResolvConf("", true)
	if strings.Contains(got, "nameserver "+IPv4Resolver+"\n") {
		t.Fatalf("IPv6-only resolver unexpectedly contains IPv4 DNS: %q", got)
	}
	for _, resolver := range []string{IPv6Resolver, IPv6ResolverBackup} {
		if !strings.Contains(got, "nameserver "+resolver+"\n") {
			t.Fatalf("IPv6-only resolver is missing %s: %q", resolver, got)
		}
	}
}

func TestInstanceResolvConfDualStack(t *testing.T) {
	got := instanceResolvConf("10.91.0.2", true)
	for _, resolver := range []string{IPv4Resolver, IPv6Resolver, IPv6ResolverBackup} {
		if !strings.Contains(got, "nameserver "+resolver+"\n") {
			t.Fatalf("dual-stack resolver is missing %s: %q", resolver, got)
		}
	}
}

func TestIncusForwardIPFallsBackToIPv6(t *testing.T) {
	if got := incusForwardIP("", "2001:db8:100::2/64"); got != "2001:db8:100::2" {
		t.Fatalf("IPv6-only forward target %q", got)
	}
	if got := incusForwardIP("10.91.0.2/20", "2001:db8:100::2/64"); got != "10.91.0.2" {
		t.Fatalf("dual-stack forward target %q, want IPv4 preference", got)
	}
}
