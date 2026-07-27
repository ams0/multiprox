# Stub Proxmox VE node image — E2E TESTING ONLY. Never deploy this.
#
# Why this exists: the real Proxmox packages are amd64-only, so on an arm64
# host (Apple silicon, Graviton) the genuine node image cannot be built or run.
# This image stands in for it so the OPERATOR's orchestration can be tested
# end-to-end on any architecture.
#
# What is real here:
#   • The operator's own generated scripts, verbatim from the ConfigMap:
#     node-bootstrap.sh, cluster-init.sh, cluster-join.sh, cluster-inventory.sh,
#     and the ceph-*.sh set. Their /etc/hosts handling, idempotency guards,
#     control flow and output parsing all execute for real.
#   • Pod identity, DNS, readiness gating, exec plumbing.
#
# What is faked:
#   • systemd — /lib/systemd/systemd sleeps instead of booting an init system.
#   • systemctl, pvecm, pveceph, ceph, pvesm — small shell stubs holding state
#     on local disk and discovering peers over the headless Service's DNS.
#
# So this validates the operator. It does NOT validate Proxmox or Ceph
# themselves; only a real amd64 cluster can do that.

FROM debian:bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive

# Utilities the operator's scripts genuinely invoke.
RUN apt-get update && apt-get install -y --no-install-recommends \
        bash \
        coreutils \
        gawk \
        sed \
        grep \
        procps \
        iproute2 \
        iputils-ping \
        openssh-server \
        passwd \
        hostname \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Fake init: node-bootstrap.sh ends with `exec /lib/systemd/systemd`, so this
# is what keeps the container alive. Everything before that exec is the real script.
#
# It also starts a REAL sshd. cluster-join.sh gates on TCP :22 before running
# pvecm add, because a genuine pvecm add exchanges credentials over SSH.
#
# A real sshd is used rather than a fake netcat listener on purpose. The stub
# previously installed netcat, which the real Proxmox image does NOT have — and
# that difference hid a genuine bug: the generated cluster-join.sh used `nc -z`,
# which failed with command-not-found on every real node while passing in e2e.
# The stub must never be more capable than the image it stands in for.
COPY fake-init /lib/systemd/systemd
RUN chmod +x /lib/systemd/systemd

COPY bin/ /usr/local/sbin/
RUN chmod +x /usr/local/sbin/*

# Directories the real scripts and PVE layout expect.
RUN mkdir -p /etc/pve /var/lib/pve-cluster /var/lib/vz /var/lib/ceph/mon /var/lib/ceph/mgr

# Marker so tests can assert they are running against the stub, never the real thing.
RUN echo "multiprox-e2e-stub" > /etc/multiprox-stub

CMD ["/lib/systemd/systemd"]
