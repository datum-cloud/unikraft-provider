#!/bin/bash -e
# Bring up the full control-plane e2e environment in a single KVM-capable
# kind cluster: cert-manager, Flux, the compute control plane, Kraftlet,
# the unikraft-provider, and the containerized Unikraft runtime — all in
# one cluster, so a compute Instance is driven end to end to a running
# microVM on the local runtime.
#
# Env:
#   PROVIDER_IMAGE          provider image, loadable by `kind load` (built by caller)
#   RUNTIME_IMAGE           ukp-runtime image, loadable by `kind load` (built by caller)
#   UKP_AGENT_CREDENTIALS   ukp.secrets.conf content (AGENT_PULL_* vendor pull
#                           credentials); the runtime pulls the test workload
#                           image with these.
#
# Produces a cluster where `chainsaw test test/e2e` (incl. run-instance)
# passes against the real runtime.
here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/../../.." && pwd)"
CLUSTER=ukp-e2e
NODE=${CLUSTER}-control-plane
CI_TOKEN=$(printf 'ci$datum.users.kraftcloud:ci-e2e-token' | base64 | tr -d '\n')
export KUBECONFIG=${KUBECONFIG:-/tmp/${CLUSTER}.kubeconfig}

log(){ printf '\n== %s\n' "$*"; }
# Use sudo only for the operations that need root (the loopback XFS mount and
# writes beneath it, the Flux CLI install); everything else runs as the
# invoking user. Empty when already root (e.g. running against a bare box).
SUDO=""; [ "$(id -u)" -eq 0 ] || SUDO="sudo"

log "host XFS with quotas at /var/lib/ukp-e2e (ukpd requires quota support)"
$SUDO umount /var/lib/ukp-e2e 2>/dev/null || true
$SUDO mkdir -p /var/lib/ukp-e2e
[ -e /ukp-e2e.img ] || { $SUDO truncate -s 10G /ukp-e2e.img; $SUDO mkfs.xfs -q /ukp-e2e.img; }
$SUDO mount -o loop,usrquota,grpquota,prjquota /ukp-e2e.img /var/lib/ukp-e2e
$SUDO mkdir -p /var/lib/ukp-e2e/data
$SUDO tee /var/lib/ukp-e2e/data/users.json >/dev/null <<JSON
[{"uuid":"aecc16c4-3a34-4f3e-9c31-f77dbbb0f68c","name":"ci","auth_token":"${CI_TOKEN}","network_id":0,"autoscale":{"min_size":0,"max_size":4},"vmdb":{"max_vcpus":4,"max_memory_mb":4096,"max_instances":16},"net":{"max_service_groups":64,"max_services":64},"vmm":{"max_vcpus":4,"max_memory_mb":4096},"stor":{"max_volumes":16,"max_volume_mb":2048,"max_total_volume_mb":8192}}]
JSON

log "create the KVM kind cluster"
kind delete cluster --name $CLUSTER >/dev/null 2>&1 || true
kind create cluster --config "$here/kind.yaml"
kind get kubeconfig --name $CLUSTER > "$KUBECONFIG"

log "load images into kind"
kind load docker-image "$RUNTIME_IMAGE" --name $CLUSTER >/dev/null
kind load docker-image "$PROVIDER_IMAGE" --name $CLUSTER >/dev/null

log "cert-manager + Flux (control-plane dependencies)"
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.16.2/cert-manager.yaml >/dev/null
command -v flux >/dev/null || curl -s https://fluxcd.io/install.sh | $SUDO bash >/dev/null 2>&1
flux install --components=source-controller,kustomize-controller,helm-controller >/dev/null
kubectl -n cert-manager wait --for=condition=Available deployment --all --timeout=180s >/dev/null

log "runtime: config + credentials, then deploy"
# The runtime pulls the test workload image from the vendor registry using
# the provided agent credentials; also bind the API on all interfaces so
# the in-cluster ukpd Service can reach it.
kubectl create namespace ukp-system --dry-run=client -o yaml | kubectl apply -f - >/dev/null
{ printf '%s\n' "$UKP_AGENT_CREDENTIALS"
  printf 'UKPD_EXTRA_ARGS+=("--api-endpoint" "0.0.0.0:45232")\n'; } > /tmp/ukp.secrets.conf
kubectl -n ukp-system create secret generic ukp-runtime-credentials \
  --from-file=ukp.secrets.conf=/tmp/ukp.secrets.conf --dry-run=client -o yaml | kubectl apply -f - >/dev/null
rm -f /tmp/ukp.secrets.conf
kubectl apply -k "$repo/config/dependencies/ukp-runtime" >/dev/null
for c in 0 1 2; do
  kubectl -n ukp-system patch ds ukp-runtime --type=json \
    -p="[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/$c/image\",\"value\":\"$RUNTIME_IMAGE\"}]" >/dev/null
done
kubectl -n ukp-system patch ds ukp-runtime --type=json \
  -p="[{\"op\":\"replace\",\"path\":\"/spec/template/spec/initContainers/0/image\",\"value\":\"$RUNTIME_IMAGE\"}]" >/dev/null
# Expose ukpd on a port-80 Service (Kraftlet derives a node label from the
# metro host, and a :port colon is an invalid label). ukpd is hostNetwork,
# so a selector-less Service + Endpoints points at the node IP:45232.
NODE_IP=$(kubectl get node ${NODE} -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')
kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Service
metadata: { name: ukpd, namespace: ukp-system }
spec: { ports: [{ name: api, port: 80, targetPort: 45232 }] }
---
apiVersion: v1
kind: Endpoints
metadata: { name: ukpd, namespace: ukp-system }
subsets:
  - addresses: [{ ip: ${NODE_IP} }]
    ports: [{ name: api, port: 45232 }]
EOF
kubectl -n ukp-system rollout status ds/ukp-runtime --timeout=180s

log "compute control plane (Flux OCIRepository is v1 in current Flux)"
tmp=$(mktemp -d); cp -r "$repo/config/dependencies/compute" "$tmp/"
sed -i 's#source.toolkit.fluxcd.io/v1beta2#source.toolkit.fluxcd.io/v1#' "$tmp/compute/ocirepository.yaml"
kubectl apply -k "$tmp/compute" >/dev/null
kubectl -n flux-system wait kustomization/compute --for=condition=Ready --timeout=240s

log "Kraftlet -> local runtime, and the provider"
kubectl create namespace kraftlet --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n kraftlet create secret generic ukc-credentials \
  --from-literal=values.yaml="$(printf 'ukc:\n  metro: %s\n  token: %s\n' 'http://ukpd.ukp-system.svc' "$CI_TOKEN")" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl apply -k "$repo/config/dependencies/kraftlet" >/dev/null
kubectl -n kraftlet wait helmrelease/kraftlet --for=condition=Ready --timeout=180s || true
kubectl apply -k "$repo/config/overlays/test-infra" >/dev/null
kubectl -n infra-provider-unikraft-system rollout status deployment/infra-provider-unikraft-controller-manager --timeout=180s

log "wait for the Kraftlet virtual node to register"
for i in $(seq 1 30); do kubectl get node kraftlet >/dev/null 2>&1 && break; sleep 4; done
kubectl get nodes
log "environment ready"
