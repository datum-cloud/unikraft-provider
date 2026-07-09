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
# Failure semantics:
#   - Missing/invalid Secret-provided users.json: lenient by design (the
#     secret volume is optional) — keep the existing/vendor-seeded file
#     rather than writing a bad one (a broken users.json crash-loops ukpd).
#   - Quota computation failure (unreadable host facts, non-numeric knobs,
#     or a reserve that meets/exceeds the host): FAIL the initContainer.
#     We never assume what a node's capacity should be.
set -eu

src=/etc/ukp-auth/users.json
dst=/var/lib/ukp/data/users.json
tmp=/tmp/users.json.seed

mkdir -p /var/lib/ukp/data

if [ ! -f "$src" ] || ! python3 -m json.tool "$src" >/dev/null 2>&1; then
  echo "seed-users: no valid generated users.json at $src; leaving existing $dst"
  exit 0
fi

# Load the UKP_QUOTA_* knobs. ukp.conf is bash-syntax (arrays), so /bin/sh
# cannot source it wholesale: extract only the simple UKP_QUOTA_*
# assignments and export them for the merge below.
set -a
eval "$(grep -E '^UKP_QUOTA_[A-Z0-9_]+=' /etc/ukp.conf 2>/dev/null || true)"
set +a

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
    instances = vmm_vcpus * knob("UKP_QUOTA_INSTANCES_PER_VCPU", 2)
    if instances < 1:
        raise ValueError(
            "vmm.max_vcpus (%d) * UKP_QUOTA_INSTANCES_PER_VCPU yields no "
            "instances" % vmm_vcpus)
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

try:
    vmm, vmdb = compute()
except Exception as exc:
    print("seed-users: ERROR: quota computation failed: %s" % exc,
          file=sys.stderr)
    sys.exit(1)

print("seed-users: computed node quotas: vmm=%s vmdb.max_instances=%d"
      % (vmm, vmdb["max_instances"]))

with open(src) as f:
    users = json.load(f)
for user in users:
    user["vmm"] = vmm
    user["vmdb"] = vmdb
with open(out, "w") as f:
    json.dump(users, f, indent=2)
    f.write("\n")
PYEOF

install -m 0600 "$tmp" "$dst"
echo "seed-users: wrote generated users.json with computed node quotas to $dst"
