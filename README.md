# kuberture

Kubernetes controller that translates EndpointSlice resources into headless Services annotated for [external-dns](https://github.com/kubernetes-sigs/external-dns) consumption.

## Problem

Kubernetes maintains an EndpointSlice (`default/kubernetes`) with control plane node IP addresses, but external-dns cannot use EndpointSlice as a source. kuberture bridges this gap:

```text
EndpointSlice (kubernetes) → kuberture → headless Service(s) with annotations → external-dns → DNS
```

## Features

- Multiple output configurations with independent hostnames, annotation prefixes, and address sources
- Four address resolution strategies (EndpointSlice direct, node internal/external/public IPs)
- Configuration hot-reload via fsnotify (no restart required for output changes)
- Server-side apply for safe, conflict-free Service management
- Prometheus metrics and health/readiness probes
- Helm chart with RBAC, ServiceMonitor, and security hardening

## Configuration

kuberture reads its configuration from a YAML file. The path is resolved in order:

1. `--config` CLI flag
2. `KUBERTURE_CONFIG` environment variable
3. `/etc/kuberture/config.yaml` (default)

### Example

```yaml
source:
  namespace: default         # EndpointSlice namespace (default: "default")
  serviceName: kubernetes    # label filter: kubernetes.io/service-name (default: "kubernetes")

outputs:
  - name: internal
    hostname:
      - api-internal.k8s.example.com
    annotationPrefix: "internal.company.io/"
    serviceName: kuberture-internal
    serviceNamespace: default
    recordTTL: 60
    addressSource: endpointslice
    addressType: IPv4

  - name: external
    hostname:
      - api.k8s.example.com
    annotationPrefix: "external-dns.alpha.kubernetes.io/"
    serviceName: kuberture-external
    serviceNamespace: default
    recordTTL: 300
    addressSource: node-external
    addressType: IPv4

metricsBindAddress: ":8080"       # default: ":8080"
healthProbeBindAddress: ":8081"   # default: ":8081"
```

### Address Sources

| Source | Description |
| --- | --- |
| `endpointslice` | IP addresses directly from EndpointSlice (default) |
| `node-internal` | `InternalIP` from `Node.status.addresses` |
| `node-external` | `ExternalIP` from `Node.status.addresses` |
| `node-public` | First non-RFC1918/RFC4193 IP from `Node.status.addresses` |

### Output Defaults

| Field | Default |
| --- | --- |
| `annotationPrefix` | `external-dns.alpha.kubernetes.io/` |
| `serviceNamespace` | `default` |
| `recordTTL` | `60` (max: 86400) |
| `addressSource` | `endpointslice` |
| `addressType` | `IPv4` |

## Installation

### Helm

```bash
helm install kuberture oci://ghcr.io/lexfrei/kuberture/charts/kuberture \
  --namespace kuberture --create-namespace \
  --values my-values.yaml
```

### Minimal values.yaml

```yaml
config:
  outputs:
    - name: api
      hostname:
        - api.k8s.example.com
      serviceName: kuberture-api
```

### Helm Values Reference

| Key | Default | Description |
| --- | --- | --- |
| `replicaCount` | `1` | Number of replicas |
| `image.repository` | `ghcr.io/lexfrei/kuberture` | Container image repository |
| `image.tag` | `""` (appVersion) | Container image tag |
| `image.pullPolicy` | `IfNotPresent` | Image pull policy |
| `serviceAccount.create` | `true` | Create ServiceAccount |
| `serviceAccount.name` | `""` | Override ServiceAccount name |
| `config` | see above | kuberture configuration |
| `resources.requests.cpu` | `10m` | CPU request |
| `resources.requests.memory` | `32Mi` | Memory request |
| `resources.limits.memory` | `64Mi` | Memory limit |
| `serviceMonitor.enabled` | `false` | Create Prometheus ServiceMonitor |
| `serviceMonitor.interval` | `30s` | Scrape interval |

## Metrics

| Metric | Type | Description |
| --- | --- | --- |
| `kuberture_reconcile_total` | Counter | Total reconciliations by status (`success`/`error`) |
| `kuberture_endpoints_resolved` | Gauge | Current resolved addresses per output |
| `kuberture_last_reconcile_timestamp` | Gauge | Unix timestamp of last successful reconciliation |

## Development

### Prerequisites

- Go 1.26+
- [golangci-lint](https://golangci-lint.run/) v2.9+
- [helm-unittest](https://github.com/helm-unittest/helm-unittest) plugin (for chart tests)

### Make Targets

```text
make build          Build binary to bin/kuberture
make test           Run tests with race detector and coverage
make lint           Run golangci-lint
make lint-fix       Run golangci-lint with --fix
make fmt            Run gofumpt and goimports
make vet            Run go vet
make tidy           Run go mod tidy
make image          Build container image
make helm-lint      Lint Helm chart
make helm-package   Package Helm chart
make e2e            Run E2E tests with kind
make all            Run fmt, vet, lint, test, build
```

### Project Structure

```text
cmd/kuberture/           Entry point
internal/
  config/                YAML parsing, validation, hot-reload watcher
  controller/            Reconciler, metrics
  resolver/              Address resolution from EndpointSlices and Nodes
e2e/                     E2E tests with kind
chart/                   Helm chart
.github/workflows/       CI/CD (lint, test, PR builds, releases)
```

## RBAC

The controller requires the following cluster-level permissions:

- `discovery.k8s.io/endpointslices`: get, list, watch
- `core/nodes`: get, list, watch
- `core/services`: get, list, watch, create, update, patch
- `coordination.k8s.io/leases`: get, list, watch, create, update, patch
- `core/events`: create, patch

## License

[BSD 3-Clause License](LICENSE)
