# syntax=docker/dockerfile:1
# Unikraft runtime (ukp) node-component image: all runtime services in one
# image, with the stock package launchers as entrypoints. The ukp packages are
# installed straight from the vendor's authenticated apt repository
# (https://pkg.unikraft.com/debian/cloud/). The repo is gated by HTTP basic
# auth; the credentials are supplied at build time via a BuildKit secret and
# never persisted into any image layer.
# See docs/enhancements/containerized-unikraft-runtime/.
#
# Build (requires BuildKit; the credentials file is Debian apt auth.conf format
# with machine/login/password lines for pkg.unikraft.com):
#   docker build --secret id=unikraft-apt-auth,src=/path/to/unikraft.conf \
#       -t ukp-runtime:TAG build/ukp-runtime/
FROM debian:trixie-slim

# Keep package postinst scripts from starting services during build.
RUN printf '#!/bin/sh\nexit 101\n' > /usr/sbin/policy-rc.d \
    && chmod +x /usr/sbin/policy-rc.d

# Dependency closure of the ukp packages (from their Depends fields), plus
# envsubst (gettext-base) used by the coredns/openresty launchers and curl
# for healthchecks. ca-certificates is also required for apt to fetch the ukp
# packages from the vendor repo over HTTPS.
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        debconf procps systemd cron libpam-modules openssl \
        python3 python3-pyroute2 python3-tqdm \
        iptables ipset iproute2 net-tools bridge-utils nftables \
        libmnl0 erofs-utils \
        gettext-base curl ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Register the vendor's authenticated apt repository. The public signing
# keyring is committed to the build context; the deb822 source references it
# via Signed-By. Credentials are NOT part of the source and are mounted only
# for the install RUN below.
#
# The vendor publishes this key nowhere public (verified: not on
# keys.openpgp.org or keyserver.ubuntu.com; the feed's key endpoints 404).
# It was extracted from /usr/share/keyrings/ on a vendor-installed host and
# is pinned here. Verify against any vendor install with:
#   gpg --show-keys --with-fingerprint unikraft-cloud.gpg
# Fingerprint: 3D6C 59FF AB8E FA6E 1025 4EFF EBD0 B225 8841 F499
#              (uid: cloud@https://pkg.unikraft.com, ProGet feed key)
COPY unikraft-cloud.gpg /usr/share/keyrings/unikraft-cloud.gpg
COPY unikraft-cloud.sources /etc/apt/sources.list.d/unikraft-cloud.sources

# Pin the ukp package versions to the set validated on the lab box, so the
# container image and the bare-metal runtime stay in lockstep and the vendor
# repo's rolling "stable" cannot silently drift our builds (which previously
# caused non-deterministic e2e failures). Bump these together when adopting a
# new vendor release; all are trixie/stable candidates in the vendor repo.
ARG UKP_AGENT_VERSION=5:0.2.4-9stable+deb13
ARG UKP_CONFIG_VERSION=5:1.4.14-26cf82c-9stable+deb13
ARG UKP_COREDNS_VERSION=5:1.14.4-13+641bea1-9stable+deb13
ARG UKP_FIRECRACKER_VERSION=5:1.16.0-31+b873e32-9stable+deb13
ARG UKP_NETWORKING_VERSION=5:1.5.6-f842be3-9stable+deb13
ARG UKP_OPENRESTY_VERSION=5:1.31.1.1-25+711fcc1-9stable+deb13
ARG UKP_PLATFORM_VERSION=5:0.11.0-2+bebb30c-9stable+deb13

# Install the ukp packages from the vendor repo. The BuildKit secret provides
# the apt basic-auth credentials at /etc/apt/auth.conf.d/unikraft.conf for the
# duration of this RUN only, so they leave no trace in any layer.
#
# The Datum-minimal profile only runs ukpd + agent + coredns, but the vendor
# packaging still forces some components into the image: ukp-platform (ukpd)
# Depends on ukp-openresty and ukp-networking at the package level, so apt
# installs them even though the DaemonSet never runs OpenResty or the firewall
# (pending a headless ukpd mode from Unikraft). What we CAN drop is ukp-lego
# (ACME/TLS) — it is only a Recommends of ukp-openresty, so with
# --no-install-recommends it stays out and Datum owns TLS anyway.
RUN --mount=type=secret,id=unikraft-apt-auth,target=/etc/apt/auth.conf.d/unikraft.conf \
    apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ukp-agent="${UKP_AGENT_VERSION}" \
        ukp-config="${UKP_CONFIG_VERSION}" \
        ukp-coredns="${UKP_COREDNS_VERSION}" \
        ukp-firecracker="${UKP_FIRECRACKER_VERSION}" \
        ukp-networking="${UKP_NETWORKING_VERSION}" \
        ukp-openresty="${UKP_OPENRESTY_VERSION}" \
        ukp-platform="${UKP_PLATFORM_VERSION}" \
    && rm -rf /var/lib/apt/lists/*

# The ukpd launcher resolves this user for storage quota ownership
# (--stor-squota-uid); create it if the packages did not.
RUN id ukpvol >/dev/null 2>&1 \
    || useradd -r -M -d /var/lib/ukp -s /usr/sbin/nologin ukpvol

# Runtime directories normally created by systemd/tmpfiles on a native install.
RUN mkdir -p /var/lib/ukp /var/log/ukp /var/run/ukp

# Datum coredns entrypoint. The stripped vendor CoreDNS has no `bind` plugin, so
# it cannot listen on just the isolate gateway IPs; its wildcard `.:53` collides
# with Talos hostDNS on 127.0.0.53:53. This wrapper runs CoreDNS on an alternate
# wildcard port and installs an nftables redirect (guest gateway:53 -> that port)
# plus forwarding to the host resolver. See docs/enhancements/.
COPY coredns-redirect.sh /usr/local/bin/coredns-redirect.sh
RUN chmod +x /usr/local/bin/coredns-redirect.sh

CMD ["/usr/lib/ukp/platform/launcher/ukpd"]
