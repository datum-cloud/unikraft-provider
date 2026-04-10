# Infra Provider Unikraft

This operator watches for Instance CRDs and creates corresponding resources in a Unikraft cluster.

## Quick Start

### Prerequisites

1. **Kubernetes cluster** - A running Kubernetes cluster where the operator will run
2. **Helm** - For installing the UKC operator
3. **Go** - For running locally during development
4. **kubectl** - For managing Kubernetes resources

### Local Setup

Follow these steps to run the operator locally in a single cluster:

#### 1. Install UKC Operator via Helm

```bash
helm install k8s-operator \
  --namespace ukc-operator \
  --create-namespace \
  --set ukc.metro=<UKC_METRO> \
  --set ukc.token=<UKC_TOKEN> \
  oci://ghcr.io/unikraft-cloud/helm-charts/k8s-operator
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

### 4. Test the Operator

In a new terminal, apply the example Instance resource:

```bash
kubectl apply -f examples/instance.yaml
```

The operator will process the Instance and create corresponding resources in the Unikraft cluster. You can verify it was created by checking:

```bash
kubectl get instances.compute.datumapis.com
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
