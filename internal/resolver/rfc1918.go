package resolver

import "net/netip"

// privatePrefixes contains all RFC1918, RFC4193, and link-local address ranges.
//
//nolint:gochecknoglobals // immutable lookup table initialized once.
var privatePrefixes = []netip.Prefix{
	// RFC1918 IPv4 private ranges.
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),

	// IPv4 link-local.
	netip.MustParsePrefix("169.254.0.0/16"),

	// IPv4 loopback.
	netip.MustParsePrefix("127.0.0.0/8"),

	// RFC4193 IPv6 Unique Local Addresses.
	netip.MustParsePrefix("fc00::/7"),

	// IPv6 link-local.
	netip.MustParsePrefix("fe80::/10"),
}

// isPrivateAddr reports whether the given parsed address belongs to a private,
// link-local, or loopback range.
func isPrivateAddr(addr netip.Addr) bool {
	for _, prefix := range privatePrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}

	return false
}

// isPrivateIP reports whether the given address string belongs to a private,
// link-local, or loopback range. It returns false for unparseable addresses.
func isPrivateIP(addr string) bool {
	parsed, err := netip.ParseAddr(addr)
	if err != nil {
		return false
	}

	return isPrivateAddr(parsed)
}
