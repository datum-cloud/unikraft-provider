#!/bin/bash -e
# Node prerequisites for running the containerized ukp stack on a bare-metal
# box. Everything here is host-level state the runtime containers assume
# (see docs/unikraft-runtime-containerization.md §6). Run as root.

echo "== kernel modules"
modprobe kvm
grep -qE 'vmx' /proc/cpuinfo && modprobe kvm_intel || true
grep -qE 'svm' /proc/cpuinfo && modprobe kvm_amd || true
modprobe tun
# Firewall prerequisites (ipset + TPROXY)
modprobe -a ip_set ip_set_hash_ip ip_set_hash_net ip_set_hash_netiface \
	xt_set xt_TPROXY xt_socket xt_mark 2>/dev/null || true
ls -l /dev/kvm /dev/net/tun

echo "== sysctls (matching the native install's 20-ukp*.conf)"
sysctl -w vm.swappiness=5
sysctl -w net.ipv4.conf.default.rp_filter=1
sysctl -w net.ipv4.conf.all.rp_filter=1
sysctl -w net.ipv4.tcp_syncookies=1
sysctl -w net.ipv4.conf.all.arp_announce=1
sysctl -w net.ipv4.conf.all.arp_ignore=0
sysctl -w net.ipv6.conf.lo.disable_ipv6=0
# NOTE: the native install also sets net.bridge.bridge-nf-call-*=0, which
# conflicts with kube-proxy/CNI expectations. Skipped here; revisit when
# the K8s layer is added to the box.

echo "== CPU governor"
if command -v cpupower >/dev/null 2>&1; then
	cpupower frequency-set -g performance || true
fi

echo "== data volume"
# The PoC does not require the full RAID/LUKS stack — any XFS mount with
# quota options at /var/lib/ukp exercises the same code paths. If no
# spare partition exists, a loopback file works:
if ! mountpoint -q /var/lib/ukp; then
	echo "no /var/lib/ukp mount — creating 20G XFS loopback with quotas"
	apt-get install -y -qq xfsprogs >/dev/null
	mkdir -p /var/lib/ukp
	if [ ! -e /ukp-data.img ]; then
		truncate -s 20G /ukp-data.img
		mkfs.xfs -q /ukp-data.img
	fi
	mount -o loop,usrquota,grpquota,prjquota /ukp-data.img /var/lib/ukp
fi
findmnt -no TARGET,SOURCE,FSTYPE,OPTIONS /var/lib/ukp

echo "== docker"
command -v docker >/dev/null 2>&1 || {
	curl -fsSL https://get.docker.com | sh
}
docker version --format 'server: {{.Server.Version}}'

echo "host bootstrap complete"
