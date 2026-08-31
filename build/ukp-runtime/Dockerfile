# syntax=docker/dockerfile:1
# Unikraft runtime (ukp) node-component image: all runtime services in one
# image, with the stock package launchers as entrypoints. The ukp packages are
# installed straight from the vendor's authenticated apt repository
# (https://pkg.unikraft.com/debian/cloud/). The repo is gated by HTTP basic
# auth; the credentials are supplied at build time via a BuildKit secret and
# never persisted into any image layer.
# See docs/enhancements/containerized-unikraft-runtime/.
#
# The kernel comes from the vendor's image registry, using a second build
# secret that holds base64 of "user:password".
#
# Build (requires BuildKit; the credentials file is Debian apt auth.conf format
# with machine/login/password lines for pkg.unikraft.com):
#   docker build --secret id=unikraft-apt-auth,src=/path/to/unikraft.conf \
#       --secret id=unikraft-registry-auth,src=/path/to/ukc-token \
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
        gettext-base curl ca-certificates jq \
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

# The vendor's staging feed, signed by its own key:
#   E157 4AA0 677C 98B7 ED46 0ED0 9E9F 3104 71E5 8DAB
#   (uid: cloud-staging@https://pkg.unikraft.com)
# It carries the builds that support images packaged without a kernel. The
# stable feed stays registered because the vendor's dependency closure still
# draws a few packages from it.
COPY unikraft-cloud-staging.gpg /usr/share/keyrings/unikraft-cloud-staging.gpg
COPY unikraft-cloud-staging.sources /etc/apt/sources.list.d/unikraft-cloud-staging.sources

# Pin the ukp package versions to the set validated on the lab box, so the
# container image and the bare-metal runtime stay in lockstep and the vendor's
# rolling feeds cannot silently drift our builds (which previously caused
# non-deterministic e2e failures). Bump these together when adopting a new
# vendor release.
#
# The whole set comes from the staging feed, which is where support for images
# packaged without a kernel lives. The two feeds package the same software
# differently: on stable, ukp-platform is a small package that pulls in
# ukp12-platform, which holds the files; on staging, ukp-platform holds them
# itself. Taking only some packages from staging installs both, and the two
# write the same files. Move the set back to stable once the vendor ships the
# feature there.
ARG UKP_AGENT_VERSION=6:0.2.5-94+2ab0038-2staging+deb13
ARG UKP_CONFIG_VERSION=6:1.4.16-eb909a6-2staging+deb13
ARG UKP_COREDNS_VERSION=6:1.14.6-18+bdd3f54-2staging+deb13
ARG UKP_FIRECRACKER_VERSION=6:1.16.1-55+db0d9f6-2staging+deb13
ARG UKP_NETWORKING_VERSION=6:1.5.10-71b92e0-2staging+deb13
ARG UKP_OPENRESTY_VERSION=6:1.31.1.1-34+2acfc15-2staging+deb13
ARG UKP_PLATFORM_VERSION=6:0.12.0-162+827997a-2staging+deb13

# ukp-remote-cni: server-side CNI shim for kraftlet.
ARG UKP_REMOTE_CNI_VERSION=6:0.1.1-19+b84b457-2staging+deb13

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
        ukp-remote-cni="${UKP_REMOTE_CNI_VERSION}" \
    && rm -rf /var/lib/apt/lists/*

# The ukpd launcher resolves this user for storage quota ownership
# (--stor-squota-uid); create it if the packages did not.
RUN id ukpvol >/dev/null 2>&1 \
    || useradd -r -M -d /var/lib/ukp -s /usr/sbin/nologin ukpvol

# Runtime directories normally created by systemd/tmpfiles on a native install.
RUN mkdir -p /var/lib/ukp /var/log/ukp /var/run/ukp

# The kernel ukpd gives to images packaged without one. It ships in the image
# rather than being downloaded on each node, so the kernel and the packages
# above update and roll back together, and no node needs a registry credential.
#
# Pinned by digest, not tag: tags move, and machines built weeks apart must not
# end up with different kernels. Update alongside UKP_PLATFORM_VERSION.
ARG UKP_KERNEL_REGISTRY=index.unikraft.io
ARG UKP_KERNEL_REPO=datum/base-compat
ARG UKP_KERNEL_MANIFEST_DIGEST=sha256:1be27d37d8f5fe19f0167aad0e9eddfb401a256e6e1ee367b140d306ce83b141
ARG UKP_KERNEL_DEST=/usr/lib/ukp/kernel

COPY fetch-kernel.sh /usr/local/bin/fetch-kernel.sh
RUN --mount=type=secret,id=unikraft-registry-auth \
    chmod +x /usr/local/bin/fetch-kernel.sh \
    && UKP_KERNEL_REGISTRY="${UKP_KERNEL_REGISTRY}" \
       UKP_KERNEL_REPO="${UKP_KERNEL_REPO}" \
       UKP_KERNEL_MANIFEST_DIGEST="${UKP_KERNEL_MANIFEST_DIGEST}" \
       UKP_KERNEL_DEST="${UKP_KERNEL_DEST}" \
       /usr/local/bin/fetch-kernel.sh

# Datum coredns entrypoint. The stripped vendor CoreDNS has no `bind` plugin, so
# it cannot listen on just the isolate gateway IPs; its wildcard `.:53` collides
# with Talos hostDNS on 127.0.0.53:53. This wrapper runs CoreDNS on an alternate
# wildcard port and installs an nftables redirect (guest gateway:53 -> that port)
# plus forwarding to the host resolver. See docs/enhancements/.
COPY coredns-redirect.sh /usr/local/bin/coredns-redirect.sh
RUN chmod +x /usr/local/bin/coredns-redirect.sh

CMD ["/usr/lib/ukp/platform/launcher/ukpd"]
