# kgpudash — Kubernetes GPU Dashboard

A real-time GPU monitoring dashboard for Kubernetes clusters, built entirely in Go.

Supports **NVIDIA**, **AMD**, and **Intel** GPUs with a live web UI served over WebSocket.
Optional historical metrics storage via **TimescaleDB/PostgreSQL**.

---

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│  Kubernetes Cluster                                          │
│                                                              │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐      │
│  │ Node (NVIDIA)│  │  Node (AMD)  │  │ Node (Intel) │      │
│  │ kgpudash-    │  │ kgpudash-    │  │ kgpudash-    │      │
│  │ agent        │  │ agent        │  │ agent        │      │
│  └──────┬───────┘  └──────┬───────┘  └──────┬───────┘      │
│         │  gRPC stream    │                  │              │
│         └────────────┬────┘──────────────────┘              │
│                      ▼                                       │
│              ┌───────────────┐                              │
│              │ kgpudash-     │◄── Kubernetes API            │
│              │ server        │    (pod-GPU mapping)         │
│              └───────┬───────┘                              │
│                      │ WebSocket                            │
│                      ▼                                       │
│              ┌───────────────┐   ┌──────────────┐          │
│              │  Web Browser  │   │ TimescaleDB  │ optional  │
│              └───────────────┘   └──────────────┘          │
└─────────────────────────────────────────────────────────────┘
```

Two binaries, one Go module:

| Binary | Role | Kubernetes resource |
|---|---|---|
| `kgpudash-agent` | Collects GPU metrics on each node | `DaemonSet` |
| `kgpudash-server` | Aggregates, enriches, serves dashboard | `Deployment` |

---

## GPU Vendor Support

| Vendor | Primary method | Fallback |
|---|---|---|
| **NVIDIA** | NVML via `go-nvml` (no subprocess) | `nvidia-smi` CSV |
| **AMD** | `/sys/class/drm/card*/device/` sysfs | `rocm-smi --json` |
| **Intel** | i915/xe sysfs | `xpu-smi dump` |

Vendor detection is automatic at agent startup. Mixed-vendor nodes are supported.

---

## Metrics

| Metric | Description |
|---|---|
| GPU Utilization % | Core compute utilization |
| VRAM Used / Total | Video memory usage |
| Temperature °C | GPU die temperature |
| Power Draw W | Current power consumption |
| Pod / Namespace | Kubernetes workload using this GPU |

Historical graphs (1h / 6h / 24h / 7d) are available when storage is enabled.

---

## Quick Start

### Prerequisites

- Go 1.22+
- Docker
- A Kubernetes cluster with GPU nodes
- `kubectl` configured

### 1. Build

```bash
# Download dependencies
go mod tidy

# Build both binaries
make build

# Or build Docker images
make docker-build
```

### 2. Push images

```bash
# Set your registry
export REGISTRY=ghcr.io/michaeltrip

make docker-push
```

### 3. Deploy to Kubernetes

```bash
# Apply RBAC, DaemonSet, Deployment, and Services
make deploy

# Check status
make status
```

### 4. Access the dashboard

```bash
# Port-forward for local access
make port-forward

# Then open http://localhost:8080
```

Or expose via LoadBalancer / Ingress — see [`deploy/service.yaml`](deploy/service.yaml).

---

## Configuration

### Agent (`kgpudash-agent`)

| Environment variable | Default | Description |
|---|---|---|
| `AGENT_GRPC_PORT` | `9090` | gRPC listen port |
| `AGENT_COLLECT_INTERVAL` | `5s` | Metric collection interval |
| `NODE_NAME` | (from Downward API) | Kubernetes node name |

### Server (`kgpudash-server`)

| Environment variable | Default | Description |
|---|---|---|
| `SERVER_HTTP_PORT` | `8080` | Web dashboard HTTP port |
| `AGENT_GRPC_PORT` | `9090` | Port agents listen on |
| `COLLECT_INTERVAL` | `5s` | Interval sent to agents |
| `KUBECONFIG` | `` | Path to kubeconfig (empty = in-cluster) |
| `NODE_IPS` | `` | Comma-separated agent node IPs |
| `STORAGE_ENABLED` | `false` | Enable TimescaleDB persistence |
| `STORAGE_DSN` | `` | PostgreSQL DSN |
| `STORAGE_RETENTION` | `30d` | Data retention period |

---

## Optional: Enable Historical Storage

1. Deploy TimescaleDB (or use a managed PostgreSQL + TimescaleDB extension):

```bash
helm repo add timescale https://charts.timescale.com
helm install timescaledb timescale/timescaledb-single \
  --namespace kgpudash \
  --set replicaCount=1
```

2. Create a Kubernetes Secret with the DSN:

```bash
kubectl -n kgpudash create secret generic kgpudash-db \
  --from-literal=dsn="postgres://user:password@timescaledb.kgpudash.svc:5432/kgpudash"
```

3. Uncomment the storage env vars in [`deploy/server-deployment.yaml`](deploy/server-deployment.yaml):

```yaml
- name: STORAGE_ENABLED
  value: "true"
- name: STORAGE_DSN
  valueFrom:
    secretKeyRef:
      name: kgpudash-db
      key: dsn
```

4. Re-apply:

```bash
make deploy
```

The schema is created automatically on first startup. Historical graphs will appear in the UI when you click a GPU card.

---

## Node Selector

By default the agent DaemonSet targets nodes with the label `gpu=true`.
Label your GPU nodes:

```bash
kubectl label node <node-name> gpu=true
```

Or remove the `nodeSelector` from [`deploy/agent-daemonset.yaml`](deploy/agent-daemonset.yaml) to run on all nodes.

---

## Regenerating Protobuf Code

```bash
# Install tools
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

# Regenerate
make proto
```

---

## Development

```bash
# Run tests
make test

# Lint
make lint

# Run agent locally (requires GPU hardware or will log "no GPUs detected")
NODE_NAME=dev-node AGENT_GRPC_PORT=9090 go run ./cmd/agent

# Run server locally (connects to a local agent)
NODE_IPS=127.0.0.1 KUBECONFIG=~/.kube/config go run ./cmd/server
```

---

## Project Structure

```
kgpudash/
├── cmd/
│   ├── agent/          # kgpudash-agent entry point
│   └── server/         # kgpudash-server entry point
├── internal/
│   ├── agent/
│   │   ├── collector/  # GPU vendor collectors (NVIDIA, AMD, Intel)
│   │   └── grpcserver/ # gRPC streaming server
│   ├── proto/gpu/      # Protobuf definitions + generated code
│   └── server/
│       ├── aggregator/ # Multi-agent stream aggregator
│       ├── k8s/        # Kubernetes pod-GPU mapper
│       ├── store/      # Metrics storage (memory + TimescaleDB)
│       └── web/        # HTTP + WebSocket server + embedded UI
├── deploy/             # Kubernetes manifests
├── Dockerfile.agent
├── Dockerfile.server
├── Makefile
└── go.mod
```

---

## License

MIT
