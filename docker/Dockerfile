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

# ── LXC guests ───────────────────────────────────────────────────────────────
# pve-container works in this environment; QEMU does not, and needs /dev/kvm on
# the host (unavailable on most cloud VMs, which do not expose nested virt).
#
# lxcfs gives guests a per-container view of /proc. LXC will not start a guest
# without it — the mount hook fails with "missing /var/lib/lxcfs/proc/ - lxcfs
# not running?" — and its unit refuses to start in a container by design:
#
#   ConditionVirtualization=!container was not met
#
# That condition guards against running lxcfs inside an LXC guest, where it
# would be meaningless. Here the "container" is the PVE node itself, which is
# precisely where lxcfs belongs, so the condition is cleared.
RUN apt-get update && apt-get install -y --no-install-recommends \
        pve-container \
        lxcfs \
        lxc-pve \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /etc/systemd/system/lxcfs.service.d \
    && printf '%s\n' \
        '[Unit]' \
        '# The node is itself a container; lxcfs is needed here, not skipped.' \
        'ConditionVirtualization=' \
        > /etc/systemd/system/lxcfs.service.d/10-multiprox-allow-container.conf \
    && systemctl enable lxcfs 2>/dev/null || true

# ── Optional: QEMU/KVM (Linux host with /dev/kvm only) ──────────────────────
# ARG ENABLE_KVM=0
# RUN if [ "$ENABLE_KVM" = "1" ]; then \
#       apt-get update && apt-get install -y --no-install-recommends \
#           qemu-server \
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
#
# systemd-udevd is deliberately NOT masked, though masking it is the usual
# container advice. Kernel uevents do reach a Docker container's namespace
# (verified with `udevadm monitor`), and two things depend on udev running:
#
#   • `rbd map` waits for udev to confirm the device before returning. With no
#     udevd the command hangs forever — the mapping succeeds in the kernel, but
#     Proxmox never gets the device path, so every LXC container on RBD hangs
#     mid-create and leaves a stuck "lock: create" behind.
#   • ceph-volume reads device properties from the udev database.
#
# It replaces the host's /run/udev, which used to be bind-mounted in for
# ceph-volume's benefit. That mount has to go: udevd cannot bind its control
# socket over a read-only one ("Address already in use").
RUN systemctl mask \
        dev-hugepages.mount \
        getty@tty1.service \
        serial-getty@.service \
        sys-fs-fuse-connections.mount \
        systemd-logind.service \
        systemd-modules-load.service \
    2>/dev/null || true \
    && systemctl disable \
        apt-daily.timer \
        apt-daily-upgrade.timer \
    2>/dev/null || true

# ── udev: create device nodes the kernel cannot ──────────────────────────────
# On a normal host /dev is devtmpfs and the KERNEL creates device nodes; udev
# only adds symlinks and properties. A container's /dev is a plain tmpfs, so
# nothing creates the node at all, and `rbd map` fails at the last step even
# with udevd running:
#
#   rbd: mapping succeeded but /dev/rbd0 is not accessible, is host /dev mounted?
#
# Mounting the host's /dev would fix it and is what the error suggests, but it
# would also shadow the container's own /dev/pts and /dev/shm — including the
# 1 GB /dev/shm that Corosync needs. Creating just the rbd nodes is enough.
# Ceph's own 50-rbd.rules then adds the /dev/rbd/<pool>/<image> symlinks that
# Proxmox looks the volume up by.
COPY scripts/rbd-mknod.sh /usr/local/bin/rbd-mknod
RUN chmod +x /usr/local/bin/rbd-mknod \
    && printf '%s\n' \
    '# multiprox: container /dev is tmpfs, not devtmpfs — create rbd nodes here.' \
    'SUBSYSTEM=="block", KERNEL=="rbd[0-9]*", ACTION=="add", RUN+="/usr/local/bin/rbd-mknod %k %M %m"' \
    'SUBSYSTEM=="block", KERNEL=="rbd[0-9]*", ACTION=="remove", RUN+="/bin/rm -f /dev/%k"' \
    > /etc/udev/rules.d/10-multiprox-rbd.rules

# Populate the node's own udev database once udevd is up. ceph-volume reads
# device properties from it and fails without them:
#
#   RuntimeError: No udev data could be retrieved for /sys/block/sdX
#
# The stock systemd-udev-trigger.service is skipped in a container, and the
# database used to come from the host's bind-mounted /run/udev, which is no
# longer mounted. Block devices only — that is all ceph-volume looks at, and a
# full trigger would replay every device on the host.
RUN printf '%s\n' \
    '[Unit]' \
    'Description=multiprox: populate the udev database for ceph-volume' \
    'After=systemd-udevd.service' \
    'Requires=systemd-udevd.service' \
    '' \
    '[Service]' \
    'Type=oneshot' \
    'RemainAfterExit=yes' \
    'ExecStart=/bin/udevadm trigger --action=add --subsystem-match=block' \
    'ExecStart=/bin/udevadm settle --timeout=30' \
    '' \
    '[Install]' \
    'WantedBy=multi-user.target' \
    > /etc/systemd/system/multiprox-udev-trigger.service \
    && systemctl enable multiprox-udev-trigger.service 2>/dev/null || true

# ── rbd unmap must retry ─────────────────────────────────────────────────────
# See scripts/rbd-unmap-retry.sh: the kernel holds an rbd device for ~20s after
# the rootfs is unmounted while writeback drains to the (loop-backed, 3x
# replicated) OSDs, and PVE's unmap_volume tries exactly once. Every container
# creation therefore rolled back at the final step, after the rootfs had already
# been built. dpkg-divert so a ceph-common upgrade cannot silently undo it.
COPY scripts/rbd-unmap-retry.sh /usr/local/share/multiprox/rbd-unmap-retry.sh
RUN if [ "$ENABLE_CEPH" = "1" ]; then \
      dpkg-divert --divert /usr/bin/rbd.distrib --rename --add /usr/bin/rbd \
      && install -m 0755 /usr/local/share/multiprox/rbd-unmap-retry.sh /usr/bin/rbd \
      && /usr/bin/rbd --version; \
    fi

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
COPY scripts/ceph-reattach.sh   /usr/local/bin/ceph-reattach
RUN chmod +x \
        /usr/local/bin/entrypoint.sh \
        /usr/local/bin/cluster-init \
        /usr/local/bin/cluster-join \
        /usr/local/bin/ceph-osd-create \
        /usr/local/bin/setup-bridge \
        /usr/local/bin/ceph-fs-create \
        /usr/local/bin/ceph-reattach

# ── Restart the node's Ceph daemons after a container recreate ───────────────
# /var/lib/ceph is a volume so the DATA survives, but the systemd units that
# `pveceph mon create` and `ceph-volume` enable are symlinks under
# /etc/systemd/system — container filesystem, gone on recreate. The node comes
# back quorate with every monitor and OSD intact and none of them running.
RUN printf '%s\n' \
    '[Unit]' \
    'Description=multiprox: restart Ceph daemons whose unit enablement was lost' \
    'After=pve-cluster.service network-online.target' \
    'Wants=pve-cluster.service' \
    '' \
    '[Service]' \
    'Type=oneshot' \
    'RemainAfterExit=yes' \
    'ExecStart=/usr/local/bin/ceph-reattach' \
    '' \
    '[Install]' \
    'WantedBy=multi-user.target' \
    > /etc/systemd/system/multiprox-ceph-reattach.service \
    && systemctl enable multiprox-ceph-reattach.service 2>/dev/null || true

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
