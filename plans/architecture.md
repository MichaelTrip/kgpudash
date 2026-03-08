# kgpudash — Kubernetes GPU Dashboard Architecture

## Overview

`kgpudash` is a Kubernetes-native GPU monitoring dashboard built entirely in Go. It consists of two binaries compiled from a single Go module:

1. **`kgpudash-agent`** — a DaemonSet that runs on every GPU node, collects GPU metrics from the host, and exposes them over gRPC to the server.
2. **`kgpudash-server`** — a Deployment that aggregates metrics from all agents, serves a real-time web dashboard over WebSocket, and optionally persists historical data to TimescaleDB/PostgreSQL.

---

## System Architecture

```mermaid
graph TD
    subgraph Kubernetes Cluster
        subgraph Node A - NVIDIA
            A1[kgpudash-agent] -->|nvidia-smi / NVML| A2[GPU 0..N]
        end
        subgraph Node B - AMD
            B1[kgpudash-agent] -->|ROCm sysfs| B2[GPU 0..N]
        end
        subgraph Node C - Intel
            C1[kgpudash-agent] -->|i915 sysfs / xpu-smi| C2[GPU 0..N]
        end

        A1 -->|gRPC stream| S[kgpudash-server]
        B1 -->|gRPC stream| S
        C1 -->|gRPC stream| S

        S -->|optional| DB[(TimescaleDB / PostgreSQL)]
        S -->|WebSocket| Browser[Web Browser]

        K8S[Kubernetes API] -->|pod-to-GPU mapping| S
    end
```

---

## Project Structure

```
kgpudash/
├── cmd/
│   ├── agent/          # main.go for kgpudash-agent
│   └── server/         # main.go for kgpudash-server
├── internal/
│   ├── agent/
│   │   ├── collector/  # GPU vendor abstraction + collection loop
│   │   │   ├── collector.go        # Collector interface
│   │   │   ├── nvidia.go           # NVIDIA via NVML / nvidia-smi
│   │   │   ├── amd.go              # AMD via ROCm sysfs / rocm-smi
│   │   │   └── intel.go            # Intel via i915 sysfs / xpu-smi
│   │   └── grpcserver/             # gRPC server exposing metrics to dashboard
│   ├── server/
│   │   ├── aggregator/             # Aggregates streams from all agents
│   │   ├── k8s/                    # Kubernetes API client (pod-GPU mapping)
│   │   ├── store/                  # Optional metrics persistence
│   │   │   ├── store.go            # Store interface
│   │   │   ├── memory.go           # In-memory ring buffer (no-storage mode)
│   │   │   └── timescale.go        # TimescaleDB/PostgreSQL backend
│   │   └── web/
│   │       ├── server.go           # HTTP + WebSocket server
│   │       ├── handlers.go         # HTTP handlers
│   │       └── static/             # Embedded HTML/CSS/JS assets
│   │           ├── index.html
│   │           ├── app.js
│   │           └── style.css
│   └── proto/                      # Protobuf definitions + generated code
│       └── gpu/
│           ├── gpu.proto
│           └── gpu.pb.go / gpu_grpc.pb.go
├── deploy/
│   ├── agent-daemonset.yaml
│   ├── server-deployment.yaml
│   ├── rbac.yaml
│   ├── service.yaml
│   └── configmap.yaml
├── Dockerfile.agent
├── Dockerfile.server
├── go.mod
├── go.sum
└── Makefile
```

---

## Component 1: kgpudash-agent (DaemonSet)

### Responsibilities
- Detect which GPU vendor(s) are present on the node at startup
- Collect GPU metrics on a configurable interval (default: 5s)
- Expose metrics via a gRPC streaming RPC to the server
- Report node name and GPU device IDs

### GPU Vendor Abstraction

```go
// internal/agent/collector/collector.go
type GPUInfo struct {
    Index       int
    UUID        string
    Name        string
    Vendor      string   // "nvidia" | "amd" | "intel"
    UtilPercent float64
    VRAMUsedMB  uint64
    VRAMTotalMB uint64
    TempCelsius float64
    PowerWatts  float64
}

type Collector interface {
    Detect() bool                    // returns true if this vendor's GPUs are present
    Collect() ([]GPUInfo, error)     // collect current snapshot
    Close() error
}
```

#### NVIDIA Collector
- Primary: use `NVML` via [`github.com/NVIDIA/go-nvml`](https://github.com/NVIDIA/go-nvml) (no subprocess)
- Fallback: parse `nvidia-smi --query-gpu=... --format=csv` output
- Metrics: utilization, memory used/total, temperature, power draw

#### AMD Collector
- Primary: read `/sys/class/drm/card*/device/gpu_busy_percent`, `/sys/class/drm/card*/device/mem_info_vram_used`, etc.
- Secondary: parse `rocm-smi --showuse --showmeminfo vram --showtemp --showpower --json` if available
- Metrics: utilization, VRAM used/total, temperature, power draw

#### Intel Collector
- Primary: read `/sys/class/drm/card*/gt/gt0/rc6_residency_ms` and `/sys/kernel/debug/dri/*/i915_frequency_info` sysfs
- Secondary: parse `xpu-smi dump` if Intel XPU Manager is available
- Metrics: utilization (GT busy), VRAM (LMEM) used/total, temperature, power draw

### gRPC Protocol

```protobuf
// internal/proto/gpu/gpu.proto
syntax = "proto3";
package gpu;

service AgentService {
  // Server calls this; agent streams back metric snapshots
  rpc StreamMetrics(StreamRequest) returns (stream MetricSnapshot);
}

message StreamRequest {
  int64 interval_ms = 1;
}

message MetricSnapshot {
  string node_name   = 1;
  int64  timestamp   = 2;  // Unix ms
  repeated GPUMetric gpus = 3;
}

message GPUMetric {
  int32  index        = 1;
  string uuid         = 2;
  string name         = 3;
  string vendor       = 4;
  double util_percent = 5;
  uint64 vram_used_mb = 6;
  uint64 vram_total_mb= 7;
  double temp_celsius = 8;
  double power_watts  = 9;
}
```

### Agent Configuration (env vars)

| Variable | Default | Description |
|---|---|---|
| `AGENT_GRPC_PORT` | `9090` | Port the agent gRPC server listens on |
| `AGENT_COLLECT_INTERVAL` | `5s` | How often to collect GPU metrics |
| `NODE_NAME` | (from k8s downward API) | Node name injected by Kubernetes |
| `AGENT_TLS_CERT` | `` | Optional TLS cert path |
| `AGENT_TLS_KEY` | `` | Optional TLS key path |

---

## Component 2: kgpudash-server (Deployment)

### Responsibilities
- Watch Kubernetes API for nodes running the agent DaemonSet pod
- Establish gRPC streaming connections to each agent
- Maintain an in-memory current-state map of all GPUs across all nodes
- Query Kubernetes API to map GPU device IDs → pods/workloads
- Serve the web dashboard (HTTP + WebSocket)
- Optionally persist snapshots to TimescaleDB

### Aggregator

The aggregator maintains a `map[nodeName]*NodeState` protected by a `sync.RWMutex`. When a new snapshot arrives from an agent, it:
1. Updates the in-memory state
2. Optionally writes to the store
3. Broadcasts the delta to all connected WebSocket clients

### Kubernetes Pod-GPU Mapping

The server uses the Kubernetes API (`client-go`) to:
- List pods with `requests/limits` for `nvidia.com/gpu`, `amd.com/gpu`, `gpu.intel.com/i915`
- Correlate pod node assignment + GPU resource requests to GPU UUIDs via node annotations set by device plugins
- Enrich GPU metrics with `pod name`, `namespace`, `container name`

RBAC required:
- `get`, `list`, `watch` on `pods` (all namespaces)
- `get`, `list`, `watch` on `nodes`

### Optional Metrics Store

```go
// internal/server/store/store.go
type Store interface {
    Write(snapshot *proto.MetricSnapshot) error
    QueryRange(node string, gpuIndex int, from, to time.Time) ([]GPUMetric, error)
    Close() error
}
```

**Memory store** (default, no storage): keeps a rolling ring buffer of the last N snapshots per GPU for sparkline display.

**TimescaleDB store** (when `STORAGE_ENABLED=true`): uses `pgx` driver to write to a hypertable:

```sql
CREATE TABLE gpu_metrics (
  time         TIMESTAMPTZ NOT NULL,
  node_name    TEXT NOT NULL,
  gpu_index    INT NOT NULL,
  gpu_uuid     TEXT,
  vendor       TEXT,
  util_percent DOUBLE PRECISION,
  vram_used_mb BIGINT,
  vram_total_mb BIGINT,
  temp_celsius DOUBLE PRECISION,
  power_watts  DOUBLE PRECISION
);
SELECT create_hypertable('gpu_metrics', 'time');
CREATE INDEX ON gpu_metrics (node_name, gpu_index, time DESC);
```

### Server Configuration (env vars)

| Variable | Default | Description |
|---|---|---|
| `SERVER_HTTP_PORT` | `8080` | Web dashboard HTTP port |
| `SERVER_GRPC_DIAL_TIMEOUT` | `10s` | Timeout connecting to agents |
| `AGENT_GRPC_PORT` | `9090` | Port agents listen on (used to build dial addresses) |
| `KUBECONFIG` | `` | Path to kubeconfig (empty = in-cluster) |
| `STORAGE_ENABLED` | `false` | Enable TimescaleDB persistence |
| `STORAGE_DSN` | `` | PostgreSQL DSN e.g. `postgres://user:pass@host/db` |
| `STORAGE_RETENTION` | `30d` | TimescaleDB data retention policy |
| `COLLECT_INTERVAL` | `5s` | Interval sent to agents |
| `SERVER_TLS_CERT` | `` | Optional TLS cert |
| `SERVER_TLS_KEY` | `` | Optional TLS key |

---

## Component 3: Web UI

### Technology
- Pure HTML5 + CSS + vanilla JavaScript
- Embedded into the Go binary via `//go:embed` (`embed.FS`)
- WebSocket connection for live metric push from server
- Chart.js for historical graphs (loaded from CDN or bundled)

### WebSocket Message Protocol (JSON)

```json
{
  "type": "snapshot",
  "timestamp": 1700000000000,
  "nodes": [
    {
      "name": "node-a",
      "gpus": [
        {
          "index": 0,
          "uuid": "GPU-abc123",
          "name": "NVIDIA A100",
          "vendor": "nvidia",
          "util_percent": 87.5,
          "vram_used_mb": 32768,
          "vram_total_mb": 40960,
          "temp_celsius": 72.0,
          "power_watts": 310.0,
          "pod": "training-job-0",
          "namespace": "ml-team",
          "container": "trainer"
        }
      ]
    }
  ]
}
```

Message types:
- `snapshot` — full current state (sent on connect + periodically)
- `delta` — only changed GPU metrics (sent on each collection cycle)
- `history` — historical data points for a selected GPU (sent on demand)

### Dashboard Layout

```
┌─────────────────────────────────────────────────────────┐
│  kgpudash  [cluster: my-cluster]          🟢 12 GPUs    │
├─────────────────────────────────────────────────────────┤
│  Filter: [All Vendors ▼]  [All Nodes ▼]  [Search pod…]  │
├──────────────┬──────────────┬──────────────┬────────────┤
│  NODE: node-a│              │              │            │
│ ┌──────────┐ │ ┌──────────┐ │ ┌──────────┐ │            │
│ │ GPU 0    │ │ │ GPU 1    │ │ │ GPU 2    │ │            │
│ │ NVIDIA   │ │ │ NVIDIA   │ │ │ NVIDIA   │ │            │
│ │ A100     │ │ │ A100     │ │ │ A100     │ │            │
│ │ Util: 87%│ │ │ Util: 12%│ │ │ Util:  0%│ │            │
│ │ VRAM:    │ │ │ VRAM:    │ │ │ VRAM:    │ │            │
│ │ 32/40 GB │ │ │  4/40 GB │ │ │  0/40 GB │ │            │
│ │ Temp: 72°│ │ │ Temp: 45°│ │ │ Temp: 38°│ │            │
│ │ Pwr: 310W│ │ │ Pwr:  85W│ │ │ Pwr:  40W│ │            │
│ │ Pod:     │ │ │ Pod:     │ │ │ Pod: idle│ │            │
│ │ train-0  │ │ │ infer-1  │ │ │          │ │            │
│ └──────────┘ │ └──────────┘ │ └──────────┘ │            │
├──────────────┴──────────────┴──────────────┴────────────┤
│  [Click GPU card to expand historical graphs]            │
│  ┌─────────────────────────────────────────────────────┐ │
│  │  GPU 0 / node-a — Last 1h  [1h][6h][24h][7d]       │ │
│  │  Utilization ████████████████░░░░░░░░░░░░░░░░░░░░  │ │
│  │  VRAM        ████████████████████████░░░░░░░░░░░░  │ │
│  │  Temperature ████████████████████░░░░░░░░░░░░░░░░  │ │
│  └─────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────┘
```

---

## Kubernetes Manifests

### RBAC (`deploy/rbac.yaml`)
- `ServiceAccount`: `kgpudash-server`
- `ClusterRole`: `get/list/watch` on `nodes`, `pods`
- `ClusterRoleBinding`: binds role to service account

### Agent DaemonSet (`deploy/agent-daemonset.yaml`)
- `hostPID: true` — needed to access GPU device files
- `privileged: true` or specific `capabilities` — needed for sysfs/NVML access
- `volumes`: mount `/dev`, `/sys`, `/proc` from host
- `env`: `NODE_NAME` via downward API
- `resources`: minimal CPU/memory requests

### Server Deployment (`deploy/server-deployment.yaml`)
- `serviceAccountName`: `kgpudash-server`
- `env`: all server config vars
- `readinessProbe` / `livenessProbe` on `/healthz`

### Service (`deploy/service.yaml`)
- `ClusterIP` service for internal agent→server gRPC
- `LoadBalancer` or `NodePort` service for web dashboard access (or `Ingress`)

---

## Key Go Dependencies

| Package | Purpose |
|---|---|
| `google.golang.org/grpc` | gRPC transport between agent and server |
| `google.golang.org/protobuf` | Protobuf serialization |
| `k8s.io/client-go` | Kubernetes API access |
| `github.com/NVIDIA/go-nvml` | NVIDIA NVML bindings |
| `github.com/jackc/pgx/v5` | PostgreSQL/TimescaleDB driver |
| `github.com/gorilla/websocket` | WebSocket server |
| `github.com/spf13/viper` | Configuration management |
| `go.uber.org/zap` | Structured logging |

---

## Data Flow

```mermaid
sequenceDiagram
    participant Agent as kgpudash-agent
    participant Server as kgpudash-server
    participant K8S as Kubernetes API
    participant DB as TimescaleDB
    participant Browser as Web Browser

    Server->>K8S: Watch for agent pods
    K8S-->>Server: Agent pod IPs
    Server->>Agent: gRPC StreamMetrics
    loop Every 5s
        Agent->>Agent: Collect GPU metrics
        Agent-->>Server: MetricSnapshot stream
        Server->>K8S: List pods with GPU resources
        K8S-->>Server: Pod-GPU mapping
        Server->>Server: Enrich metrics with pod info
        opt Storage enabled
            Server->>DB: Write snapshot
        end
        Server-->>Browser: WebSocket delta push
    end
    Browser->>Server: Request history for GPU X
    opt Storage enabled
        Server->>DB: QueryRange
        DB-->>Server: Historical rows
    end
    Server-->>Browser: history message
```

---

## Implementation Phases

### Phase 1 — Core Infrastructure
1. Initialize Go module (`go mod init github.com/MichaelTrip/kgpudash`)
2. Define protobuf schema and generate Go code
3. Implement `Collector` interface + NVIDIA collector (NVML + smi fallback)
4. Implement agent gRPC server
5. Implement server aggregator + gRPC client
6. Basic HTTP server with static file embedding

### Phase 2 — Multi-vendor + Kubernetes Integration
7. Implement AMD collector (sysfs + rocm-smi)
8. Implement Intel collector (sysfs + xpu-smi)
9. Implement Kubernetes pod-GPU mapping
10. WebSocket broadcast of live metrics

### Phase 3 — Web UI
11. Build GPU card grid layout (HTML/CSS)
12. WebSocket client in JavaScript
13. Live metric updates with smooth animations
14. Vendor-specific color coding (green=NVIDIA, red=AMD, blue=Intel)

### Phase 4 — Optional Storage
15. Define `Store` interface + memory ring buffer
16. Implement TimescaleDB store with `pgx`
17. Historical graph UI with Chart.js
18. Time range selector (1h / 6h / 24h / 7d)

### Phase 5 — Kubernetes Deployment
19. Write Dockerfiles for agent and server
20. Write Kubernetes manifests (DaemonSet, Deployment, RBAC, Service)
21. Write Makefile with `build`, `docker-build`, `deploy` targets
22. Write README with deployment instructions
