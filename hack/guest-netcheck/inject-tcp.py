import sys
from scapy.all import *
conf.verb = 0
iface, hmac, gmac, src, dst = sys.argv[1:6]
p = (Ether(src=hmac, dst=gmac) /
     IPv6(src=src, dst=dst, hlim=64) /
     TCP(sport=44444, dport=8080, flags="S", seq=1000))
sendp(p, iface=iface, count=3, inter=0.8)
print("injected TCP SYN %s -> %s:8080 on %s" % (src, dst, iface))
