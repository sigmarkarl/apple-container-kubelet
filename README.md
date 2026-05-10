# apple-container-kubelet

A [Virtual Kubelet](https://virtual-kubelet.io/) provider that runs Kubernetes pods on macOS using Apple's native [`container`](https://developer.apple.com/documentation/virtualization) CLI (available in macOS 26+). Each pod container is backed by an Apple container, giving you Kubernetes-managed workloads on Apple Silicon Macs.

## Why this vs kind / k3d / colima / minikube?

Those tools spin up a **local Kubernetes cluster on your Mac**, usually inside a single Linux VM (or Docker Desktop). `apple-container-kubelet` solves a different problem: it makes your Mac a **node in any existing cluster** (your team's dev cluster, a kind cluster on the same machine, anything reachable). Pods scheduled to it run as **native Apple containers** — one lightweight VM per container, on the same Virtualization.framework that powers macOS 26.

|                          | apple-container-kubelet | kind / k3d / minikube | colima / Docker Desktop |
| ------------------------ | ----------------------- | --------------------- | ----------------------- |
| Runtime                  | Apple `container` CLI   | Docker / containerd in a Linux VM | Docker in a Linux VM |
| Adds Mac to *your* cluster | ✅                    | ❌ (creates a new local one) | ❌ |
| Per-container isolation  | Lightweight VM each     | Shared VM, namespaced | Shared VM, namespaced |
| Native arm64, no QEMU    | ✅                      | ✅                    | ✅ |
| Requires Docker          | ❌                      | ❌ (kind via podman possible) | ✅ |
| First-party Apple runtime | ✅                     | ❌                    | ❌ |

**When to reach for it:** you want a remote/local Kubernetes cluster to schedule workloads onto your Mac directly — for CI, edge dev, or burst capacity — using Apple's runtime instead of Docker.

**When not to:** you just want a throwaway local cluster for `kubectl apply` testing — `kind` is still the simplest answer there.

## Features

- **Virtual Kubelet node** — registers a virtual node (`darwin/arm64`) in your Kubernetes cluster
- **Pod lifecycle** — create, update, delete pods; init containers run sequentially before main containers
- **Container restart policies** — supports `Always`, `OnFailure`, and `Never` with CrashLoopBackOff
- **Liveness and readiness probes** — exec, HTTP GET, and TCP socket probes
- **`kubectl logs`** — streams container logs (including `--follow` and `--previous`)
- **`kubectl exec`** — interactive exec into containers via SPDY with TTY support
- **Environment variables** — resolves `env`, `envFrom` (ConfigMap/Secret refs), and `fieldRef`
- **Volume mounts** — EmptyDir, HostPath, ConfigMap, and Secret volumes
- **Resource limits** — maps container CPU/memory limits to VM vCPUs and memory
- **mTLS** — optional client certificate verification using the cluster CA
- **Heartbeat** — periodic node status updates to keep the node Ready

## Prerequisites

- macOS 26 (Tahoe) or later on Apple Silicon
- The `container` CLI (`/usr/bin/container`) — ships with macOS 26
- Go 1.26+ (to build from source)
- Access to a Kubernetes cluster (via kubeconfig or in-cluster config)

## Installing

### Homebrew (macOS, Apple Silicon)

```bash
brew tap sigmarkarl/tap
brew install apple-container-kubelet
```

### Building from source

```bash
make build
```

The binary is written to `bin/apple-container-kubelet`.

## Configuration

The kubelet reads configuration from `/etc/apple-container-kubelet/config.toml` by default. A sample config is provided in [`config/default.toml`](config/default.toml):

```toml
[resources]
vcpus = 0       # 0 = use Apple container defaults
memory_mib = 0  # 0 = use Apple container defaults

[tls]
# Path to cluster CA cert for mTLS (leave empty to disable)
client_ca_path = ""

[debug]
enabled = false
```

Container resource limits specified in the pod spec override the defaults above.

### Setting up mTLS

To enable mTLS so only your cluster's API server can reach the kubelet:

```bash
./scripts/update-client-ca.sh [/path/to/config.toml]
```

This extracts the CA certificate from your current kubeconfig context and updates the config file.

## Running

```bash
# Use default kubeconfig (~/.kube/config)
bin/apple-container-kubelet

# Or specify a kubeconfig
KUBECONFIG=/path/to/kubeconfig bin/apple-container-kubelet

# Override the node name
APPLEVM_NODE_NAME=my-mac bin/apple-container-kubelet
```

The kubelet will:
1. Register a virtual node in the cluster with the taint `virtual-kubelet.io/provider=applevm:NoSchedule`
2. Watch for pods scheduled to the node
3. Start an HTTPS server on port 10250 for `kubectl logs` and `kubectl exec`

## Scheduling pods

Pods must tolerate the taint and select the virtual node:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: my-app
spec:
  tolerations:
  - key: "virtual-kubelet.io/provider"
    value: "applevm"
    effect: "NoSchedule"
  nodeSelector:
    node.kubernetes.io/applevm: "true"
  containers:
  - name: app
    image: docker.io/library/alpine:latest
    command: ["sleep", "3600"]
```

## Testing

```bash
# Unit tests
make test

# Integration tests (requires the `container` CLI)
make test-integration

# End-to-end tests (requires a running cluster with the kubelet registered)
make test-e2e
```

## Architecture

```
cmd/containerd-shim-applevm-v2/   Entry point — node registration, heartbeat, wiring
pkg/provider/                      Pod lifecycle, container status, probes, restart logic
pkg/hypervisor/                    Abstraction over the Apple `container` CLI
pkg/server/                        Kubelet HTTPS server (logs, exec via SPDY)
config/                            TOML configuration loading
```

## License

MIT
