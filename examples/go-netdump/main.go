// go-netdump reports a guest's network state from inside the unikernel.
//
// It prints a full report to stdout at boot -- the console is the only channel
// that still works when the guest has no usable route -- and serves the same
// report over HTTP for re-querying a guest whose networking does work.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// procNetFiles are read verbatim when present; a unikernel's procfs is minimal
// or absent, so a missing file is reported, not treated as an error.
var procNetFiles = []string{
	"/proc/net/dev",
	"/proc/net/route",
	"/proc/net/ipv6_route",
	"/proc/net/if_inet6",
	"/proc/net/arp",
	"/proc/net/ndisc",
	"/etc/resolv.conf",
	"/etc/hosts",
}

// defaultRouteProbes force a route lookup for each scope that matters: a global
// address needs a default route, a ULA needs the VPC route, link-local needs
// only an on-link interface.
var defaultRouteProbes = []string{
	"[2001:4860:4860::8888]:53",
	"[fd20::1]:53",
	"8.8.8.8:53",
}

type Report struct {
	Time       string      `json:"time"`
	Hostname   string      `json:"hostname"`
	Env        []string    `json:"env"`
	Interfaces []Iface     `json:"interfaces"`
	IfaceError string      `json:"interfaceError,omitempty"`
	RouteTable []Route     `json:"routeTable"`
	RouteError string      `json:"routeTableError,omitempty"`
	Neighbours []Neighbour `json:"neighbours"`
	NeighError string      `json:"neighbourError,omitempty"`
	Routes     []RouteInfo `json:"routeProbes"`
	Reach      []ReachInfo `json:"tcpProbes,omitempty"`
	ProcFiles  []ProcFile  `json:"files"`
}

// Route is one entry of the guest's kernel routing table.
type Route struct {
	Family   string `json:"family"`
	Dst      string `json:"dst"`
	DstLen   int    `json:"dstLen"`
	Gateway  string `json:"gateway,omitempty"`
	PrefSrc  string `json:"prefSrc,omitempty"`
	Oif      string `json:"oif,omitempty"`
	OifIndex int    `json:"oifIndex,omitempty"`
	Table    int    `json:"table"`
	Priority int    `json:"priority,omitempty"`
	Protocol int    `json:"protocol"`
	Scope    int    `json:"scope"`
	Type     string `json:"type"`
}

type Neighbour struct {
	Family string `json:"family"`
	Addr   string `json:"addr"`
	LLAddr string `json:"llAddr,omitempty"`
	Iface  string `json:"iface,omitempty"`
	State  string `json:"state"`
}

type Iface struct {
	Name   string   `json:"name"`
	Index  int      `json:"index"`
	MTU    int      `json:"mtu"`
	MAC    string   `json:"mac,omitempty"`
	Flags  string   `json:"flags"`
	Addrs  []string `json:"addrs"`
	Errors string   `json:"error,omitempty"`
}

// RouteInfo is one route lookup. A connectionless UDP dial performs the
// lookup and source selection without putting a packet on the wire, so it
// answers "does this guest have a route to X, and what source would it use"
// even when nothing on the far end is listening.
type RouteInfo struct {
	Target string `json:"target"`
	OK     bool   `json:"ok"`
	Source string `json:"source,omitempty"`
	Error  string `json:"error,omitempty"`
}

type ReachInfo struct {
	Target  string `json:"target"`
	OK      bool   `json:"ok"`
	Latency string `json:"latency,omitempty"`
	Source  string `json:"source,omitempty"`
	Error   string `json:"error,omitempty"`
}

type ProcFile struct {
	Path     string `json:"path"`
	Present  bool   `json:"present"`
	Contents string `json:"contents,omitempty"`
	Error    string `json:"error,omitempty"`
}

func collect() Report {
	r := Report{Time: time.Now().UTC().Format(time.RFC3339), Env: relevantEnv()}
	r.Hostname, _ = os.Hostname()

	// net.Interfaces goes through netlink, which a unikernel may not implement;
	// the failure itself is a finding, so record it and keep going.
	ifaces, err := net.Interfaces()
	if err != nil {
		r.IfaceError = err.Error()
	}
	for _, ifc := range ifaces {
		e := Iface{Name: ifc.Name, Index: ifc.Index, MTU: ifc.MTU, Flags: ifc.Flags.String()}
		if len(ifc.HardwareAddr) > 0 {
			e.MAC = ifc.HardwareAddr.String()
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			e.Errors = err.Error()
		}
		for _, a := range addrs {
			e.Addrs = append(e.Addrs, a.String())
		}
		r.Interfaces = append(r.Interfaces, e)
	}

	if routes, err := dumpRoutes(); err != nil {
		r.RouteError = err.Error()
	} else {
		r.RouteTable = routes
	}
	if nb, err := dumpNeighbours(); err != nil {
		r.NeighError = err.Error()
	} else {
		r.Neighbours = nb
	}

	for _, t := range probeTargets() {
		r.Routes = append(r.Routes, probeRoute(t))
	}
	for _, t := range splitList(os.Getenv("NETDUMP_TCP_TARGETS")) {
		r.Reach = append(r.Reach, probeTCP(t))
	}
	for _, p := range procNetFiles {
		r.ProcFiles = append(r.ProcFiles, readFile(p))
	}
	return r
}

func probeTargets() []string {
	if extra := splitList(os.Getenv("NETDUMP_TARGETS")); len(extra) > 0 {
		return append(append([]string{}, defaultRouteProbes...), extra...)
	}
	return defaultRouteProbes
}

func probeRoute(target string) RouteInfo {
	c, err := net.Dial("udp", target)
	if err != nil {
		return RouteInfo{Target: target, Error: err.Error()}
	}
	defer c.Close() //nolint:errcheck // probe socket, nothing was written
	return RouteInfo{Target: target, OK: true, Source: c.LocalAddr().String()}
}

func probeTCP(target string) ReachInfo {
	start := time.Now()
	c, err := net.DialTimeout("tcp", target, tcpTimeout())
	if err != nil {
		return ReachInfo{Target: target, Error: err.Error(), Latency: time.Since(start).String()}
	}
	defer c.Close() //nolint:errcheck // probe socket
	return ReachInfo{Target: target, OK: true, Latency: time.Since(start).String(), Source: c.LocalAddr().String()}
}

func tcpTimeout() time.Duration {
	if v := os.Getenv("NETDUMP_TCP_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return 5 * time.Second
}

func readFile(path string) ProcFile {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ProcFile{Path: path}
		}
		return ProcFile{Path: path, Error: err.Error()}
	}
	return ProcFile{Path: path, Present: true, Contents: string(b)}
}

func relevantEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "NETDUMP_") || strings.HasPrefix(kv, "UK") {
			out = append(out, kv)
		}
	}
	sort.Strings(out)
	return out
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func render(r Report, w *strings.Builder) {
	fmt.Fprintf(w, "=== go-netdump %s  hostname=%s\n", r.Time, r.Hostname)
	if len(r.Env) > 0 {
		fmt.Fprintf(w, "env: %s\n", strings.Join(r.Env, " "))
	}

	fmt.Fprintf(w, "\n--- interfaces\n")
	if r.IfaceError != "" {
		fmt.Fprintf(w, "  ENUMERATION FAILED: %s\n", r.IfaceError)
	}
	if len(r.Interfaces) == 0 && r.IfaceError == "" {
		fmt.Fprintf(w, "  (none reported)\n")
	}
	for _, i := range r.Interfaces {
		fmt.Fprintf(w, "  %s idx=%d mtu=%d mac=%s flags=%s\n", i.Name, i.Index, i.MTU, orDash(i.MAC), i.Flags)
		if i.Errors != "" {
			fmt.Fprintf(w, "      addr error: %s\n", i.Errors)
		}
		for _, a := range i.Addrs {
			fmt.Fprintf(w, "      %s\n", a)
		}
	}

	fmt.Fprintf(w, "\n--- route table\n")
	if r.RouteError != "" {
		fmt.Fprintf(w, "  DUMP FAILED: %s\n", r.RouteError)
	}
	if len(r.RouteTable) == 0 && r.RouteError == "" {
		fmt.Fprintf(w, "  (empty)\n")
	}
	for _, rt := range r.RouteTable {
		fmt.Fprintf(w, "  %-6s %-34s via %-26s dev %-10s table=%d metric=%d %s\n",
			rt.Family, fmt.Sprintf("%s/%d", rt.Dst, rt.DstLen), orDash(rt.Gateway),
			orDash(rt.Oif), rt.Table, rt.Priority, rt.Type)
	}

	fmt.Fprintf(w, "\n--- neighbours\n")
	if r.NeighError != "" {
		fmt.Fprintf(w, "  DUMP FAILED: %s\n", r.NeighError)
	}
	if len(r.Neighbours) == 0 && r.NeighError == "" {
		fmt.Fprintf(w, "  (empty)\n")
	}
	for _, n := range r.Neighbours {
		fmt.Fprintf(w, "  %-40s %-18s dev %-10s %s\n", n.Addr, orDash(n.LLAddr), orDash(n.Iface), n.State)
	}

	fmt.Fprintf(w, "\n--- route probes (does a route exist, and which source is chosen)\n")
	for _, p := range r.Routes {
		if p.OK {
			fmt.Fprintf(w, "  OK    %-28s src=%s\n", p.Target, p.Source)
		} else {
			fmt.Fprintf(w, "  FAIL  %-28s %s\n", p.Target, p.Error)
		}
	}

	if len(r.Reach) > 0 {
		fmt.Fprintf(w, "\n--- tcp reachability\n")
		for _, p := range r.Reach {
			if p.OK {
				fmt.Fprintf(w, "  OK    %-28s %s src=%s\n", p.Target, p.Latency, p.Source)
			} else {
				fmt.Fprintf(w, "  FAIL  %-28s %s after %s\n", p.Target, p.Error, p.Latency)
			}
		}
	}

	fmt.Fprintf(w, "\n--- files\n")
	for _, f := range r.ProcFiles {
		if !f.Present {
			reason := "absent"
			if f.Error != "" {
				reason = f.Error
			}
			fmt.Fprintf(w, "  %-22s %s\n", f.Path, reason)
			continue
		}
		fmt.Fprintf(w, "  %-22s\n", f.Path)
		for _, line := range strings.Split(strings.TrimRight(f.Contents, "\n"), "\n") {
			fmt.Fprintf(w, "      %s\n", line)
		}
	}
	fmt.Fprintf(w, "=== end\n")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func dumpToLog() {
	var b strings.Builder
	render(collect(), &b)
	// Written as one Write so the report cannot interleave with other output.
	_, _ = os.Stdout.WriteString(b.String())
}

func main() {
	dumpToLog()

	// Re-dumping on a timer is how you watch state arrive (a Router
	// Advertisement installing a default route, say) on a guest you cannot
	// reach over the network yet.
	if v := os.Getenv("NETDUMP_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			go func() {
				for range time.Tick(d) {
					dumpToLog()
				}
			}()
		} else {
			log.Printf("ignoring NETDUMP_INTERVAL=%q: %v", v, err)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/text", func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		render(collect(), &b)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(b.String()))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(collect())
	})

	addr := "0.0.0.0:" + port()
	log.Printf("go-netdump listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func port() string {
	if p := os.Getenv("PORT"); p != "" {
		if _, err := strconv.Atoi(p); err == nil {
			return p
		}
	}
	return "8080"
}
