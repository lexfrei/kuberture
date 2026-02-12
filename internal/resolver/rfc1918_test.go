package resolver

import "testing"

func Test_isPrivateIP(t *testing.T) {
	tests := []struct {
		name string
		addr string
		want bool
	}{
		{name: "RFC1918 10.x", addr: "10.0.0.1", want: true},
		{name: "RFC1918 10.x high", addr: "10.255.255.255", want: true},
		{name: "RFC1918 172.16.x", addr: "172.16.0.1", want: true},
		{name: "RFC1918 172.31.x", addr: "172.31.255.255", want: true},
		{name: "RFC1918 192.168.x", addr: "192.168.1.1", want: true},
		{name: "link-local IPv4", addr: "169.254.1.1", want: true},
		{name: "loopback IPv4", addr: "127.0.0.1", want: true},
		{name: "RFC4193 IPv6 ULA", addr: "fd00::1", want: true},
		{name: "link-local IPv6", addr: "fe80::1", want: true},
		{name: "public IPv4", addr: "203.0.113.10", want: false},
		{name: "public IPv4 8.8.8.8", addr: "8.8.8.8", want: false},
		{name: "public IPv6", addr: "2001:db8::1", want: false},
		{name: "172.15.x not private", addr: "172.15.255.255", want: false},
		{name: "172.32.x not private", addr: "172.32.0.1", want: false},
		{name: "unparseable returns false", addr: "not-an-ip", want: false},
		{name: "empty string returns false", addr: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isPrivateIP(tt.addr)
			if got != tt.want {
				t.Errorf("isPrivateIP(%q) = %v, want %v", tt.addr, got, tt.want)
			}
		})
	}
}
