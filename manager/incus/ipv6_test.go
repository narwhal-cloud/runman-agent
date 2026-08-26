//go:build linux

package incus

import "testing"

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
