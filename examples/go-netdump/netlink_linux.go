//go:build linux

package main

import (
	"fmt"
	"net"
	"syscall"
)

// dumpRoutes reads the kernel routing table over NETLINK_ROUTE. This is the
// same interface net.Interfaces() uses, and a unikernel may implement neither;
// the returned error is itself the finding, so callers report it rather than
// treating an empty table as "no routes".
func dumpRoutes() ([]Route, error) {
	msgs, err := netlinkDump(syscall.RTM_GETROUTE)
	if err != nil {
		return nil, err
	}
	names := ifaceNames()
	var out []Route
	for _, m := range msgs {
		if m.Header.Type != syscall.RTM_NEWROUTE {
			continue
		}
		rt := (*syscall.RtMsg)(unsafePtr(m.Data))
		attrs, err := syscall.ParseNetlinkRouteAttr(&m)
		if err != nil {
			continue
		}
		r := Route{
			Family:   familyName(rt.Family),
			DstLen:   int(rt.Dst_len),
			Table:    int(rt.Table),
			Protocol: int(rt.Protocol),
			Scope:    int(rt.Scope),
			Type:     routeTypeName(rt.Type),
		}
		for _, a := range attrs {
			switch a.Attr.Type {
			case syscall.RTA_DST:
				r.Dst = ipString(a.Value)
			case syscall.RTA_GATEWAY:
				r.Gateway = ipString(a.Value)
			case syscall.RTA_PREFSRC:
				r.PrefSrc = ipString(a.Value)
			case syscall.RTA_OIF:
				idx := int(nativeUint32(a.Value))
				r.OifIndex = idx
				r.Oif = names[idx]
			case syscall.RTA_PRIORITY:
				r.Priority = int(nativeUint32(a.Value))
			case syscall.RTA_TABLE:
				r.Table = int(nativeUint32(a.Value))
			}
		}
		if r.Dst == "" {
			r.Dst = defaultDst(rt.Family)
		}
		out = append(out, r)
	}
	return out, nil
}

// dumpNeighbours reads the neighbour cache, which tells you whether the guest
// ever resolved its gateway -- the usual reason an otherwise-correct route
// forwards nothing.
func dumpNeighbours() ([]Neighbour, error) {
	msgs, err := netlinkDump(syscall.RTM_GETNEIGH)
	if err != nil {
		return nil, err
	}
	names := ifaceNames()
	var out []Neighbour
	for _, m := range msgs {
		if m.Header.Type != syscall.RTM_NEWNEIGH {
			continue
		}
		if len(m.Data) < sizeofNdMsg {
			continue
		}
		nd := (*ndMsg)(unsafePtr(m.Data))
		// syscall.ParseNetlinkRouteAttr rejects RTM_NEWNEIGH, so walk the
		// attributes directly.
		attrs, err := parseAttrs(m.Data[sizeofNdMsg:])
		if err != nil {
			continue
		}
		n := Neighbour{
			Family: familyName(nd.Family),
			Iface:  names[int(nd.Ifindex)],
			State:  neighStateName(nd.State),
		}
		for _, a := range attrs {
			switch a.Attr.Type {
			case ndaDst:
				n.Addr = ipString(a.Value)
			case ndaLLAddr:
				n.LLAddr = net.HardwareAddr(a.Value).String()
			}
		}
		out = append(out, n)
	}
	return out, nil
}

func netlinkDump(proto int) ([]syscall.NetlinkMessage, error) {
	b, err := syscall.NetlinkRIB(proto, syscall.AF_UNSPEC)
	if err != nil {
		return nil, fmt.Errorf("netlink dump: %w", err)
	}
	msgs, err := syscall.ParseNetlinkMessage(b)
	if err != nil {
		return nil, fmt.Errorf("parse netlink response: %w", err)
	}
	return msgs, nil
}

func ifaceNames() map[int]string {
	names := map[int]string{}
	ifaces, err := net.Interfaces()
	if err != nil {
		return names
	}
	for _, i := range ifaces {
		names[i.Index] = i.Name
	}
	return names
}

func ipString(b []byte) string {
	if len(b) != net.IPv4len && len(b) != net.IPv6len {
		return fmt.Sprintf("%x", b)
	}
	return net.IP(b).String()
}

func defaultDst(family uint8) string {
	if family == syscall.AF_INET {
		return "0.0.0.0"
	}
	return "::"
}

func familyName(f uint8) string {
	switch f {
	case syscall.AF_INET:
		return "inet"
	case syscall.AF_INET6:
		return "inet6"
	}
	return fmt.Sprintf("af%d", f)
}

func routeTypeName(t uint8) string {
	switch t {
	case syscall.RTN_UNICAST:
		return "unicast"
	case syscall.RTN_LOCAL:
		return "local"
	case syscall.RTN_BROADCAST:
		return "broadcast"
	case syscall.RTN_MULTICAST:
		return "multicast"
	case syscall.RTN_BLACKHOLE:
		return "blackhole"
	case syscall.RTN_UNREACHABLE:
		return "unreachable"
	case syscall.RTN_PROHIBIT:
		return "prohibit"
	}
	return fmt.Sprintf("type%d", t)
}

func neighStateName(s uint16) string {
	states := []struct {
		bit  uint16
		name string
	}{
		{0x01, "INCOMPLETE"}, {0x02, "REACHABLE"}, {0x04, "STALE"},
		{0x08, "DELAY"}, {0x10, "PROBE"}, {0x20, "FAILED"},
		{0x40, "NOARP"}, {0x80, "PERMANENT"},
	}
	for _, st := range states {
		if s&st.bit != 0 {
			return st.name
		}
	}
	return fmt.Sprintf("state%d", s)
}

// ndMsg mirrors the kernel's struct ndmsg, which package syscall does not
// export.
type ndMsg struct {
	Family  uint8
	Pad1    uint8
	Pad2    uint16
	Ifindex int32
	State   uint16
	Flags   uint8
	Type    uint8
}

const (
	sizeofNdMsg = 12
	ndaDst      = 1
	ndaLLAddr   = 2
)

func parseAttrs(b []byte) ([]syscall.NetlinkRouteAttr, error) {
	var out []syscall.NetlinkRouteAttr
	for len(b) >= syscall.SizeofRtAttr {
		a := (*syscall.RtAttr)(unsafePtr(b))
		if int(a.Len) < syscall.SizeofRtAttr || int(a.Len) > len(b) {
			return out, fmt.Errorf("malformed netlink attribute")
		}
		out = append(out, syscall.NetlinkRouteAttr{
			Attr:  *a,
			Value: b[syscall.SizeofRtAttr:a.Len],
		})
		b = b[rtaAlign(int(a.Len)):]
	}
	return out, nil
}

func rtaAlign(n int) int { return (n + 3) &^ 3 }
