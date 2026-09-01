import sys
from scapy.all import *
conf.verb = 0
iface, hmac, gmac, src, dst = sys.argv[1:6]
p = (Ether(src=hmac, dst=gmac) /
     IPv6(src=src, dst=dst, hlim=64) /
     ICMPv6EchoRequest(id=0x4242, seq=1, data=b"XCELL-PROOF"))
sendp(p, iface=iface, count=5, inter=0.6)
print("injected %s -> %s on %s" % (src, dst, iface))
