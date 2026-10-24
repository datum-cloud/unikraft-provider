package imageconvert

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	dockerconfig "github.com/containerd/containerd/v2/core/remotes/docker/config"
)

// Egress decides which registries the adapter talks to. Image references
// come from users, and the adapter runs in the node's network namespace, so
// without it a reference could point the adapter at the node's loopback
// services, the cloud metadata endpoint or the cluster network.
type Egress struct {
	// Insecure lists registries, as host[:port], reached over plain HTTP and
	// exempt from the address checks, e.g. a registry for local testing.
	Insecure []string
	// Denied holds addresses refused on top of the built-in special-purpose
	// ranges, such as the node's own addresses and the cluster's CIDRs.
	Denied []netip.Prefix
}

// deniedRanges are special-purpose ranges netip has no predicate for.
var deniedRanges = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),
}

// LocalPrefixes returns the addresses of the node's interfaces, which may be
// public yet reach services bound to the node.
func LocalPrefixes() ([]netip.Prefix, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var prefixes []netip.Prefix
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if ip, ok := netip.AddrFromSlice(ipnet.IP); ok {
			prefixes = append(prefixes, netip.PrefixFrom(ip.Unmap(), ip.Unmap().BitLen()))
		}
	}
	return prefixes, nil
}

// allowed reports whether the adapter may connect to addr, an IP address
// and port as dialed, after name resolution.
func (e *Egress) allowed(addr string) error {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		return fmt.Errorf("registry address %q: %w", addr, err)
	}
	ip := ap.Addr().Unmap()
	denied := ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified()
	for _, p := range append(deniedRanges, e.Denied...) {
		denied = denied || p.Contains(ip)
	}
	if denied {
		return fmt.Errorf("registry address %s is not allowed", ip)
	}
	return nil
}

// hosts returns the registry configuration for the given credentials: HTTPS
// through a dialer that checks every address it connects to, including those
// of token servers and redirects, or plain HTTP for the insecure registries.
func (e *Egress) hosts(creds Credentials) docker.RegistryHosts {
	credentials := func(string) (string, string, error) {
		return creds.Username, creds.Password, nil
	}
	guarded := dockerconfig.ConfigureHosts(context.Background(), dockerconfig.HostOptions{
		Credentials:   credentials,
		DefaultScheme: "https",
		UpdateClient: func(client *http.Client) error {
			tr, ok := client.Transport.(*http.Transport)
			if !ok {
				return fmt.Errorf("unexpected registry transport %T", client.Transport)
			}
			tr = tr.Clone()
			// A proxy would be dialed instead of the registry, bypassing the
			// address checks.
			tr.Proxy = nil
			tr.DialContext = (&net.Dialer{
				Timeout:       30 * time.Second,
				KeepAlive:     30 * time.Second,
				FallbackDelay: 300 * time.Millisecond,
				Control: func(_, address string, _ syscall.RawConn) error {
					return e.allowed(address)
				},
			}).DialContext
			client.Transport = tr
			return nil
		},
	})
	insecure := dockerconfig.ConfigureHosts(context.Background(), dockerconfig.HostOptions{
		Credentials:   credentials,
		DefaultScheme: "http",
	})
	return func(host string) ([]docker.RegistryHost, error) {
		if slices.Contains(e.Insecure, host) {
			return insecure(host)
		}
		return guarded(host)
	}
}

func (e *Egress) resolver(creds Credentials) remotes.Resolver {
	return docker.NewResolver(docker.ResolverOptions{Hosts: e.hosts(creds)})
}
