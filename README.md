# Infra Provider Unikraft

This operator watches for Instance CRDs and creates corresponding Kubernetes Pods scheduled to a Kraftlet node.

## Kraftlet

Unikraft Cloud integrates seamlessly with any Kubernetes cluster through a virtual kubelet known as Kraftlet. More information in [Unikraft docs](https://unikraft.com/docs/integrations/kubernetes).

### Prerequisites

1. **Kubernetes cluster** - A running Kubernetes cluster for Kraftlet to join as node
2. **Helm** - For installing Kraftlet
3. **Go** - For running locally during development
4. **kubectl** - For managing Kubernetes resources

### 1. Install Kraftlet via Helm

```bash
helm install kraftlet \
  --namespace kraftlet \
  --create-namespace \
  --set ukc.metro=$UKC_METRO \
  --set ukc.token=$UKC_TOKEN \
  --set image.tag=0.4.0 \
  --set kraftlet.podSyncWorkers=64 \
  --set kraftlet.podStatusUpdateInterval="5s" \
  oci://ghcr.io/unikraft-cloud/helm-charts/kraftlet
```

#### 2. Install Datum CRDs

```bash
make install
```

This will install the Workload and Instance CRDs into your cluster.

#### 3. Run the Operator Locally

```bash
go run ./cmd/main.go --server-config=config/dev/example-same-cluster.yaml
```

The operator is now running and will watch for CRD changes in your cluster.

### 4. Apply Kubernetes resources

You can apply a Kubernetes Service and a Deployment to a cluster. Check the `examples/kraftlet` for resource configuration.

```bash
kubectl apply -f examples/instance.yaml
```

Pods are scheduled to Kraftlet via nodeSelector. Kraftlet then deploys each container defined by a pod as an instance, and creates a UKC service based on the Kubernetes service configuration.


### 5. Check live Datum Instance

You can apply a Kubernetes Service and a Deployment to a cluster. Check the `examples/kraftlet` for resource configuration.

```bash
kubectl get instances.compute.datumapis.com
```

Should return:

```
NAME                       AGE     READY   REASON     NETWORK IP   EXTERNAL IP
example-sandbox-instance   4m57s   True    PodReady   10.0.0.9 
```

## Testing

End-to-end tests run against a hermetic kind cluster provisioned by
[`datum-cloud/test-infra`](https://github.com/datum-cloud/test-infra) and
driven by [Task](https://taskfile.dev) + [Chainsaw](https://kyverno.github.io/chainsaw/).

### Prerequisites

- `docker`, `kind`
- `task` (>= 3.x), with `TASK_X_REMOTE_TASKFILES=1` exported
- `chainsaw`
- Downstream CRDs for `github.com/unikraft-cloud/k8s-operator` dropped into
  `config/dependencies/unikraft-k8s-operator-crds/` (the upstream repo is
  private so the CRDs cannot be fetched automatically)

### Local flow

```bash
export TASK_X_REMOTE_TASKFILES=1
task test:setup    # kind up, install CRDs, build + load image, deploy provider
task e2e           # run chainsaw tests
task test:teardown # delete kind cluster
```
