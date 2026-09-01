import sys, threading, time
from scapy.all import *
conf.verb = 0
iface, hmac, gmac, gaddr = sys.argv[1:5]
# Each source address probes a different routing decision in the guest: can it
# build a return path to that scope?
sources = sys.argv[5:]
seen = {}
def sniffer():
    def cb(p):
        if ICMPv6EchoReply in p:
            seen[p[IPv6].dst] = p[ICMPv6EchoReply].seq
    sniff(iface=iface, prn=cb, timeout=len(sources)*1.2 + 4, store=0,
          lfilter=lambda p: ICMPv6EchoReply in p)
t = threading.Thread(target=sniffer); t.start()
time.sleep(1)
for i, s in enumerate(sources):
    sendp(Ether(src=hmac, dst=gmac)/IPv6(src=s, dst=gaddr, hlim=64)/
          ICMPv6EchoRequest(id=0x5150, seq=i+1, data=b"SWEEP"), iface=iface)
    time.sleep(0.4)
t.join()
print("%-42s %s" % ("SOURCE (what the guest must route back to)", "REPLIED?"))
for s in sources:
    print("%-42s %s" % (s, "yes" if s in seen else "NO"))
