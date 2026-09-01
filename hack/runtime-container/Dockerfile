# Prototype image for running the Unikraft Cloud platform (ukp) as containers.
# Packages are fetched from an existing installation (see debs/) rather than
# the authenticated apt repo; see docs/unikraft-runtime-containerization.md.
FROM debian:trixie-slim

# Keep package postinst scripts from starting services during build.
RUN printf '#!/bin/sh\nexit 101\n' > /usr/sbin/policy-rc.d \
    && chmod +x /usr/sbin/policy-rc.d

# Dependency closure of the ukp packages (from their Depends fields), plus
# envsubst (gettext-base) used by the coredns/openresty launchers and curl
# for healthchecks.
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        debconf procps systemd cron libpam-modules openssl \
        python3 python3-pyroute2 python3-tqdm \
        iptables ipset iproute2 net-tools bridge-utils nftables \
        libmnl0 erofs-utils \
        gettext-base curl ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY debs/*.deb /tmp/debs/
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends /tmp/debs/*.deb \
    && rm -rf /tmp/debs /var/lib/apt/lists/*

# The ukpd launcher resolves this user for storage quota ownership
# (--stor-squota-uid); create it if the packages did not.
RUN id ukpvol >/dev/null 2>&1 \
    || useradd -r -M -d /var/lib/ukp -s /usr/sbin/nologin ukpvol

# Runtime directories normally created by systemd/tmpfiles on a native install.
RUN mkdir -p /var/lib/ukp /var/log/ukp /var/run/ukp

CMD ["/usr/lib/ukp/platform/launcher/ukpd"]
