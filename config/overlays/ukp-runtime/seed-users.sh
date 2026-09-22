#!/bin/sh
# Seed ukpd's user database from the ESO-generated secret (mounted at
# /etc/ukp-auth), merging in per-node quotas computed from host resources.
# Runs as the seed-users initContainer (this file is mounted from the
# ukp-seed-script ConfigMap).
#
# The secret carries identity + permissions only; the vmdb/vmm quotas are
# computed here from the host's real resources (the pod is hostNetwork on
# Talos, so nproc and /proc/meminfo report the host, not a cgroup view).
# Knobs come from /etc/ukp.conf (UKP_QUOTA_*).
#
# Fail closed, like activate-node: a node that cannot seed its credential must
# not serve guests. ukpd authenticates image and platform-kernel resolution as
# this user, so a user with no auth_token leaves every instance unable to
# resolve any image, long after this container has exited.
#
# Failure semantics — all FAIL the initContainer:
#   - No Secret-provided users.json, or one that does not parse.
#   - A user record with no auth_token. A structurally valid file is not
#     enough; the token is the whole point of seeding.
#   - Quota computation failure (unreadable host facts, non-numeric knobs,
#     or a reserve that meets/exceeds the host). We never assume what a
#     node's capacity should be.
set -eu

src=/etc/ukp-auth/users.json
dst=/var/lib/ukp/data/users.json
tmp=/tmp/users.json.seed

mkdir -p /var/lib/ukp/data

# An absent file means the Secret is missing, or present without the key the
# volume selects. Both are configuration errors, not a reason to start ukpd on
# whatever happens to be on the data volume.
if [ ! -f "$src" ]; then
  echo "seed-users: FATAL: no $src" >&2
  echo "seed-users: the ukp-auth volume selects key 'users.json' from Secret" >&2
  echo "seed-users: kraftlet-ukc-token; check that the Secret exists and has" >&2
  echo "seed-users: that key, and that its ExternalSecret is synced." >&2
  exit 1
fi

if ! python3 -m json.tool "$src" >/dev/null 2>&1; then
  echo "seed-users: FATAL: $src does not parse as JSON" >&2
  echo "seed-users: check the users.json template on ExternalSecret" >&2
  echo "seed-users: kraftlet-ukc-token." >&2
  exit 1
fi

# Load the UKP_QUOTA_* knobs. ukp.conf is bash-syntax (arrays), so /bin/sh
# cannot source it wholesale: extract only the simple UKP_QUOTA_*
# assignments. Each is a DEFAULT the node inherits unless the same variable is
# already set in the environment (e.g. a per-node DaemonSet env override) — an
# explicit env value wins, so a small node can lower a reserve without editing
# the conf that prod bare-metal shares.
while IFS= read -r assignment; do
  [ -n "$assignment" ] || continue
  name=${assignment%%=*}
  eval "current=\${$name:-}"
  [ -n "$current" ] || eval "export $assignment"
done <<EOF
$(grep -E '^UKP_QUOTA_[A-Z0-9_]+=' /etc/ukp.conf 2>/dev/null || true)
EOF

# Merge per-node quotas into the generated identity. These quotas double as
# the platform's capacity enforcement: the node's full resources minus a
# fixed reserve, which is what keeps guests from starving ukpd, the agent,
# and Talos system components. Any computation failure exits non-zero and
# (via set -e) fails the initContainer.
python3 - "$src" "$tmp" <<'PYEOF'
import json, os, sys

src, out = sys.argv[1], sys.argv[2]

def knob(name, default):
    val = os.environ.get(name, "").strip()
    return int(val) if val else default

def compute():
    # UKP_QUOTA_HOST_CPUS / UKP_QUOTA_MEMINFO are test/override hooks; on a
    # hostNetwork pod the defaults see the real host.
    cpus = knob("UKP_QUOTA_HOST_CPUS", os.cpu_count() or 0)
    if cpus < 1:
        raise ValueError("could not determine host cpu count")
    mem_mb = 0
    with open(os.environ.get("UKP_QUOTA_MEMINFO") or "/proc/meminfo") as f:
        for line in f:
            if line.startswith("MemTotal:"):
                mem_mb = int(line.split()[1]) // 1024
                break
    if mem_mb < 1:
        raise ValueError("could not determine host memory")
    cpu_reserve = knob("UKP_QUOTA_CPU_RESERVE", 4)
    mem_reserve = knob("UKP_QUOTA_MEM_RESERVE_MB", 8192)
    vmm_vcpus = cpus - cpu_reserve
    if vmm_vcpus <= 0:
        raise ValueError(
            "host cpus (%d) - UKP_QUOTA_CPU_RESERVE (%d) leaves no vcpus "
            "for guests; fix the reserve for this node" % (cpus, cpu_reserve))
    vmm_mem = mem_mb - mem_reserve
    if vmm_mem <= 0:
        raise ValueError(
            "host memory (%d MB) - UKP_QUOTA_MEM_RESERVE_MB (%d) leaves no "
            "memory for guests; fix the reserve for this node"
            % (mem_mb, mem_reserve))
    # Pods capacity is intentionally a high static ceiling: CPU and memory
    # (the vmm quotas) are the binding scheduling constraints. The vendor
    # documents max_instances scaling to very large values (the practical
    # bound is controller DB footprint).
    instances = knob("UKP_QUOTA_MAX_INSTANCES", 5000)
    if instances < 1:
        raise ValueError(
            "UKP_QUOTA_MAX_INSTANCES (%d) must be positive" % instances)
    vmm = {"max_vcpus": vmm_vcpus, "max_memory_mb": vmm_mem}
    vmdb = {
        "max_instances": instances,
        "min_memory_mb": 16,
        "def_memory_mb": 128,
        "max_memory_mb": knob("UKP_QUOTA_VM_MAX_MEM_MB", 8192),
        "min_vcpus": 1,
        "max_vcpus": knob("UKP_QUOTA_VM_MAX_VCPUS", 8),
    }
    return vmm, vmdb

with open(src) as f:
    users = json.load(f)

# ukpd authenticates every image and platform-kernel resolution as the user,
# so a record without a token yields a runtime that starts and then fails to
# resolve anything. Catch it here rather than hours later at instance start.
if not users:
    print("seed-users: ERROR: %s contains no users" % src, file=sys.stderr)
    sys.exit(1)
for user in users:
    if not str(user.get("auth_token") or "").strip():
        print("seed-users: ERROR: user %s (%s) in %s has no auth_token; check "
              "the users.json template on ExternalSecret kraftlet-ukc-token "
              "and that its generated password resolved"
              % (user.get("uuid", "?"), user.get("name", "?"), src),
              file=sys.stderr)
        sys.exit(1)

try:
    vmm, vmdb = compute()
except Exception as exc:
    print("seed-users: ERROR: quota computation failed: %s" % exc,
          file=sys.stderr)
    sys.exit(1)

print("seed-users: computed node quotas: vmm=%s vmdb.max_instances=%d"
      % (vmm, vmdb["max_instances"]))

for user in users:
    user["vmm"] = vmm
    user["vmdb"] = vmdb
with open(out, "w") as f:
    json.dump(users, f, indent=2)
    f.write("\n")
PYEOF

install -m 0600 "$tmp" "$dst"
echo "seed-users: wrote generated users.json with computed node quotas to $dst"
