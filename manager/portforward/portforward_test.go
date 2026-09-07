package portforward

import "testing"

func TestFormatTargetAddress(t *testing.T) {
	tests := []struct {
		name string
		ip   string
		port int
		want string
	}{
		{name: "IPv4", ip: "10.91.0.2", port: 22, want: "10.91.0.2:22"},
		{name: "IPv6", ip: "2001:db8:100::2", port: 22, want: "[2001:db8:100::2]:22"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatTargetAddress(tc.ip, tc.port); got != tc.want {
				t.Fatalf("formatTargetAddress(%q, %d) = %q, want %q", tc.ip, tc.port, got, tc.want)
			}
		})
	}
}
