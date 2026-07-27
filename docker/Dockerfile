############################################################################
# multiprox — Proxmox VE management node (containerised)
#
# Base: Debian Bookworm + Proxmox VE packages (no kernel).
#
# What runs inside this container:
#   • pvedaemon / pveproxy  → REST API + web UI on :8006
#   • pve-cluster / pmxcfs  → cluster filesystem at /etc/pve (via Corosync)
#   • corosync              → quorum and messaging between nodes
#   • sshd                  → required for pvecm join and live migration
#
# What does NOT run (no kernel, no /dev/kvm on Mac Docker Desktop):
#   • KVM-accelerated VMs   → needs /dev/kvm on the host
#   • Nested LXC containers → needs privileged + lxcfs
#
# To enable VM/LXC capability (Linux host with KVM only):
#   Uncomment the QEMU/LXC block below and set ENABLE_KVM=1 in .env
############################################################################

# ── Base image: the Debian suite decides which Proxmox you get ───────────────
# Proxmox builds its packages per Debian release, so this single line selects
# the entire stack:
#
#   bookworm -> PVE 8.4.x, Ceph reef 18.x / squid 19.x
#   trixie   -> PVE 9.2.x, Ceph squid 19.x / tentacle 20.x   <- current
#
# ceph-tentacle is published for trixie ONLY, so latest Ceph requires trixie.
# Changing this line means changing the PVE repo suite and the release key
# below to match — they are a set.
#
# NOT the -slim variant. Slim ships
#   /etc/dpkg/dpkg.cfg.d/docker  ->  path-exclude /usr/share/doc/*
# and pve-manager keeps a *data* file there — /usr/share/doc/pve-manager/aplinfo.dat,
# the appliance-template index. dpkg discards it at unpack and the postinst dies:
#
#   cp: cannot stat '/usr/share/doc/pve-manager/aplinfo.dat': No such file or directory
#   dpkg: error processing package pve-manager (--configure)
#
# The whole install aborts. A targeted `path-include` would also work, but
# Proxmox assumes an ordinary Debian across ~350 packages and many postinst
# scripts, and slim strips man pages and locales too — the full base is safer.
# It costs roughly 100 MB on an image that is already ~2 GB.
FROM debian:trixie

LABEL org.opencontainers.image.title="multiprox node" \
      org.opencontainers.image.description="Proxmox VE cluster node (containerised)" \
      org.opencontainers.image.source="https://github.com/feynman/multiprox"

ENV DEBIAN_FRONTEND=noninteractive

# ── Base OS ──────────────────────────────────────────────────────────────────
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        wget \
        gnupg \
        iproute2 \
        iputils-ping \
        net-tools \
        openssh-server \
        openssh-client \
        procps \
        psmisc \
        systemd \
        systemd-sysv \
        dbus \
        sshpass \
    && rm -rf /var/lib/apt/lists/*

# ── Proxmox VE repository (no-subscription tier) ─────────────────────────────
# The release key is published per suite, so it tracks the base image above.
RUN wget -qO /etc/apt/trusted.gpg.d/proxmox-release-trixie.gpg \
        https://enterprise.proxmox.com/debian/proxmox-release-trixie.gpg \
    && echo "deb [arch=amd64] http://download.proxmox.com/debian/pve trixie pve-no-subscription" \
         > /etc/apt/sources.list.d/pve.list \
    && apt-get update

# ── Proxmox VE — management + cluster layer (no kernel) ─────────────────────
RUN apt-get install -y --no-install-recommends \
        pve-manager \
        pve-cluster \
        corosync \
        libpve-http-server-perl \
        pve-firewall \
    && rm -rf /var/lib/apt/lists/*

# ── Disable the enterprise repository ────────────────────────────────────────
# Installing pve-manager drops in /etc/apt/sources.list.d/pve-enterprise.list:
#
#   deb https://enterprise.proxmox.com/debian/pve bookworm pve-enterprise
#
# That repo requires a paid subscription, so the NEXT `apt-get update` in this
# build (the Ceph stage) fails hard:
#
#   Err https://enterprise.proxmox.com/debian/pve bookworm InRelease
#     401  Unauthorized
#   E: The repository '...' is not signed.
#
# We already use the no-subscription repo, so drop any enterprise entry. Matching
# on content rather than filename covers both the .list and deb822 .sources forms
# that different Proxmox versions ship.
RUN grep -rlZ 'enterprise\.proxmox\.com' /etc/apt/sources.list.d/ 2>/dev/null \
      | xargs -0 -r rm -f \
    && echo "remaining apt sources:" && ls /etc/apt/sources.list.d/

# ── Optional: QEMU/KVM and LXC (Linux host with /dev/kvm only) ──────────────
# Uncomment if you have a Linux host and want full VM + container support.
# ARG ENABLE_KVM=0
# RUN if [ "$ENABLE_KVM" = "1" ]; then \
#       apt-get update && apt-get install -y --no-install-recommends \
#           qemu-server \
#           pve-container \
#       && rm -rf /var/lib/apt/lists/*; \
#     fi

# ── Optional: Ceph (required by the Kubernetes operator's spec.ceph) ─────────
# Build with --build-arg ENABLE_CEPH=1 to install the Proxmox Ceph packages so
# `pveceph` works inside the container.
#
# lvm2 is essential, not incidental: `pveceph osd create` delegates to
# ceph-volume, which creates an LVM PV/VG/LV on the raw block device it is
# handed. Without lvm2 the OSD creation fails.
#
# gdisk/parted let ceph-volume zap and re-label devices on re-use.
ARG ENABLE_CEPH=0
RUN if [ "$ENABLE_CEPH" = "1" ]; then \
      echo "deb [arch=amd64] http://download.proxmox.com/debian/ceph-tentacle trixie no-subscription" \
        > /etc/apt/sources.list.d/ceph.list \
      && apt-get update \
      && apt-get install -y --no-install-recommends \
           ceph \
           ceph-common \
           ceph-mon \
           ceph-mgr \
           ceph-osd \
           # ceph-mds backs CephFS, which is how a Proxmox cluster shares ISOs,
           # CT templates and snippets — RBD is block-only and cannot be mounted
           # on several nodes at once, so it cannot serve that role.
           ceph-mds \
           ceph-fuse \
           ceph-volume \
           lvm2 \
           gdisk \
           parted \
           nvme-cli \
      && rm -rf /var/lib/apt/lists/*; \
    fi

# ── LVM must not wait on udev inside a container ─────────────────────────────
# `pveceph osd create` delegates to ceph-volume, which builds an LVM stack on
# the target device. LVM normally asks udev to create the LV's device node and
# then waits for it; there is no udev running in this container, so the wait
# fails and OSD creation aborts with:
#
#   /dev/<vg>/<lv>: not found: device not cleared
#   Aborting. Failed to wipe start of new LV.
#
# This is written as a drop-in rather than by editing lvm.conf, because Debian
# ships every one of these settings COMMENTED OUT (`# udev_sync = 1`). A
# sed that rewrites `udev_sync = 1` therefore matches nothing and silently
# changes nothing — the failure still happens. lvmlocal.conf is read after
# lvm.conf specifically to hold local overrides, so these values win.
#
# Written with printf, NOT a heredoc. Dockerfile heredocs require BuildKit; on
# the legacy builder `cat > file <<'EOF'` silently creates an EMPTY file and the
# build still succeeds. That produced an image that looked fine and failed at
# `pveceph osd create` — the worst kind of failure. printf works on both builders.
RUN mkdir -p /etc/lvm && printf '%s\n' \
    '# multiprox: allow LVM to operate without udev (containerised PVE node).' \
    'activation {' \
    '    udev_sync = 0' \
    '    udev_rules = 0' \
    '    verify_udev_operations = 0' \
    '}' \
    'devices {' \
    '    obtain_device_list_from_udev = 0' \
    '}' \
    > /etc/lvm/lvmlocal.conf \
    && echo "--- lvmlocal.conf ---" && cat /etc/lvm/lvmlocal.conf \
    && lvmconfig activation/udev_sync devices/obtain_device_list_from_udev

# Belt and braces: device-mapper also consults this. lvmlocal.conf is the
# primary fix because it is read from disk on every LVM invocation and so
# survives systemd sanitising the environment of the services it spawns.
ENV DM_DISABLE_UDEV=1

# ── Systemd container hygiene ─────────────────────────────────────────────────
# Disable units that either don't apply or conflict inside a container.
RUN systemctl mask \
        dev-hugepages.mount \
        getty@tty1.service \
        serial-getty@.service \
        sys-fs-fuse-connections.mount \
        systemd-logind.service \
        systemd-udevd.service \
        systemd-modules-load.service \
    2>/dev/null || true \
    && systemctl disable \
        apt-daily.timer \
        apt-daily-upgrade.timer \
    2>/dev/null || true

# ── SSH daemon ────────────────────────────────────────────────────────────────
# pvecm join SSHs from new nodes into node1 to exchange credentials.
RUN mkdir -p /var/run/sshd \
    && sed -i \
        -e 's/#PermitRootLogin.*/PermitRootLogin yes/' \
        -e 's/#PasswordAuthentication.*/PasswordAuthentication yes/' \
        /etc/ssh/sshd_config \
    && ssh-keygen -A

# Default root password — override via ROOT_PASSWORD env var in entrypoint.
# CHANGE THIS before exposing to any network.
RUN echo 'root:proxmox' | chpasswd

# ── Network interfaces ────────────────────────────────────────────────────────
# A placeholder only. The real file is generated at boot by setup-bridge, which
# is the only thing that knows which interface Docker put on the guest network
# and what address it was given.
#
# What matters here is that this file is SAFE if ifupdown reads it before that
# happens: it declares nothing but loopback. An earlier version described eth0
# as DHCP and vmbr0 as a bridge over it, which is what a bare-metal node looks
# like and exactly wrong here — there is no DHCP server on a Docker network, and
# enslaving eth0 would cut the node off from the cluster mid-boot.
COPY config/interfaces /etc/network/interfaces

# ── PVE datacenter defaults ───────────────────────────────────────────────────
# Staged OUTSIDE /etc/pve, deliberately.
#
# /etc/pve is where pmxcfs (the cluster filesystem) mounts itself via FUSE, and
# FUSE refuses to mount over a non-empty directory:
#
#   fuse: mountpoint is not empty
#   [main] crit: fuse_mount error: File exists
#   pve-cluster.service: Failed with result 'exit-code'
#
# Putting anything in /etc/pve at image-build time therefore prevents the
# cluster filesystem from ever starting, which in turn breaks every pvecm and
# pveceph command. cluster-init.sh copies this into place after pmxcfs mounts.
COPY config/datacenter.cfg /usr/share/multiprox/datacenter.cfg

# /etc/pve must exist and be EMPTY — it is only ever a mountpoint.
RUN mkdir -p /etc/pve && rm -rf /etc/pve/* /etc/pve/.[!.]* 2>/dev/null || true

# ── Scripts ───────────────────────────────────────────────────────────────────
COPY scripts/entrypoint.sh      /usr/local/bin/entrypoint.sh
COPY scripts/cluster-init.sh    /usr/local/bin/cluster-init
COPY scripts/cluster-join.sh    /usr/local/bin/cluster-join
COPY scripts/ceph-osd-create.sh /usr/local/bin/ceph-osd-create
COPY scripts/setup-bridge.sh    /usr/local/bin/setup-bridge
COPY scripts/ceph-fs-create.sh  /usr/local/bin/ceph-fs-create
RUN chmod +x \
        /usr/local/bin/entrypoint.sh \
        /usr/local/bin/cluster-init \
        /usr/local/bin/cluster-join \
        /usr/local/bin/ceph-osd-create \
        /usr/local/bin/setup-bridge \
        /usr/local/bin/ceph-fs-create

# ── Persistent volumes ────────────────────────────────────────────────────────
# /etc/pve        → cluster filesystem (pmxcfs, managed by pve-cluster)
# /var/lib/pve-cluster → corosync DB + auth keys
# /var/lib/vz     → VM images, ISO templates, backups
# /var/log/pve    → PVE-specific logs
VOLUME ["/var/lib/pve-cluster", "/var/lib/vz", "/var/log/pve"]

# ── Ports ─────────────────────────────────────────────────────────────────────
# 8006       PVE web UI / REST API (HTTPS)
# 22         SSH (pvecm join, migration)
# 5405-5412  Corosync (knet, UDP)
# 111        rpcbind / portmapper (pmxcfs)
EXPOSE 8006 22 5405-5412/udp 111

# systemd shuts down cleanly on SIGRTMIN+3
STOPSIGNAL SIGRTMIN+3

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
