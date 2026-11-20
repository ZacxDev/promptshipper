package shared

import (
	"net/netip"
	"net/url"
	"strings"
)

// CleartextRejection says why rawURL may not be used, or "" when it may.
//
// 🔴 THE SAME RULE AS THE ANDROID APP (android/…/PrivateNetwork.kt), so a
// pairing payload one client accepts the other does too: https:// is always
// allowed; plain http:// only to `localhost` or an IP literal in 10/8,
// 172.16/12, 192.168/16, 100.64/10 (CGNAT — Tailscale), 127/8, ::1 or
// fd00::/8. A HOSTNAME over http is refused even when it resolves privately:
// what it resolves to is decided by whatever network the laptop is on now,
// and the bearer token would travel in cleartext to that answer.
//
// Only the scheme and host are judged; the URL's shape is validateBaseURL's.
// The message names the host, never a token (a URL carries none: userinfo is
// refused before this runs on the pairing path, and is never quoted here).
func CleartextRejection(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "is not a URL"
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return ""
	case "http":
	default:
		return "must start with http:// or https://"
	}
	host := u.Hostname()
	if host == "" {
		return "has no host"
	}
	if isPrivateHost(host) {
		return ""
	}
	return "plain http:// is only allowed to a private IP address " +
		"(10.x, 172.16–31.x, 192.168.x, 100.64–127.x, 127.x, ::1, fd00::/8) — use https:// for " + host
}

// isPrivateHost: `localhost`, or a strict IP literal in the ranges above.
// netip refuses leading-zero octets ("010.0.0.1" is octal to some parsers);
// a zone id (fd00::1%eth0) and an IPv4-mapped IPv6 address (::ffff:10.0.0.1)
// are refused, as the app does, with no clause of their own: netip's
// Prefix.Contains never matches a zoned address and a zoned ::1 is not ::1,
// and a mapped address is neither IPv4 nor inside ::1 or fd00::/8. (An
// explicit clause for either was mutation-tested and found unreachable.)
func isPrivateHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	if a.Is4() {
		for _, p := range privateV4 {
			if p.Contains(a) {
				return true
			}
		}
		return false
	}
	return a == netip.IPv6Loopback() || fd00.Contains(a)
}

var (
	privateV4 = []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"),
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("127.0.0.0/8"),
	}
	fd00 = netip.MustParsePrefix("fd00::/8")
)
