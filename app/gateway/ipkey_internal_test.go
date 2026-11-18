package gateway

import "testing"

// Una IPv6 se agrupa por su /64; una IPv4 (y lo que no parsea) queda igual.
func TestIPKey(t *testing.T) {
	cases := map[string]string{
		"203.0.113.7":          "ip:203.0.113.7",
		"2001:db8:1:2::1":      "ip6:2001:db8:1:2::/64",
		"2001:db8:1:2:ffff::9": "ip6:2001:db8:1:2::/64",
		"2001:db8:1:3::1":      "ip6:2001:db8:1:3::/64",
		"::ffff:203.0.113.7":   "ip:::ffff:203.0.113.7",
		"no-es-ip":             "ip:no-es-ip",
	}
	for in, want := range cases {
		if got := ipKey(in); got != want {
			t.Errorf("ipKey(%q) = %q, esperaba %q", in, got, want)
		}
	}
}
