package adminauth

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ClientIP is the address the rate limiter and the audit row use (D6). It is RemoteAddr's host,
// except when trustProxy is set and RemoteAddr is loopback (a local TLS proxy such as tailscale
// serve or Caddy): then it is the LAST X-Forwarded-For hop, the one that proxy appended. Earlier
// hops are client-supplied and never trusted. A missing or unparseable hop keeps RemoteAddr. An
// IPv6 zone (fe80::1%en0) is dropped: the address goes into an inet column.
func ClientIP(r *http.Request, trustProxy bool) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	remote, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	host = remote.WithZone("").String()
	if !trustProxy || !remote.IsLoopback() {
		return host
	}
	xff := r.Header.Values("X-Forwarded-For")
	if len(xff) == 0 {
		return host
	}
	hops := strings.Split(xff[len(xff)-1], ",")
	last, err := netip.ParseAddr(strings.TrimSpace(hops[len(hops)-1]))
	if err != nil {
		return host
	}
	return last.WithZone("").String()
}
