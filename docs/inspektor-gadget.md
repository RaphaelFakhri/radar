# Inspektor Gadget Live Debug

Radar's Live Debug feature uses an existing Inspektor Gadget DaemonSet to
collect bounded kernel evidence for one Pod container. Radar never installs
Gadget from the UI or status endpoint. Processes and connections are one-shot
snapshots; DNS is a 10, 30, or 60 second capture.

## Security boundary

The Kubernetes API server authorizes the Gadget Pod proxy connection using the
current Radar user's impersonated identity. In local mode it uses the current
kubeconfig identity. A run fails closed if Radar cannot construct that config.

Inspektor Gadget's authorization boundary is node-wide, not workload-wide. An
identity that can proxy to a Gadget Pod can ask that daemon to instrument other
workloads on the same node. Pod-level RBAC does not narrow this. Treat Gadget
proxy access as privileged diagnostic access.

The minimum namespace-scoped grant verified against Inspektor Gadget v0.54.1 is:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: radar-live-debug
  namespace: gadget
rules:
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list"]
  - apiGroups: [""]
    resources: ["pods/portforward"]
    verbs: ["create"]
```

Bind this only to operators who may collect node-level kernel evidence. Radar
also needs ordinary `get pods` access in the target workload namespace to pin
the Pod UID, node, container name, and runtime container ID.

## Artifact policy

Radar pins the three curated OCI gadgets by multi-platform manifest digest for
each release. `--ig-gadget-registry` changes only the registry prefix for a
mirror; it does not fall back to tags or another digest. Configure Gadget's
daemon-side allowed-gadgets and signature policy to allow those exact mirrored
references. Radar surfaces policy and pull errors without substituting a less
restricted artifact.

## Compatibility

An installed DaemonSet does not prove that every node can load eBPF programs.
Live Debug checks that the target node's Gadget Pod is Ready, then preserves the
runtime's concrete compatibility error. Linux nodes generally need kernel 5.10+
with BTF. GKE Autopilot, Fargate, and Docker Desktop kernels without BTF may be
unsupported even though detection reports `installed`.

Every result records the pinned Pod UID, node, container ID, capture window,
row/event count, Radar truncation, and whether IG-side event loss was measurable.
DNS absence is phrased as "no response observed in the capture window," never as
a proven timeout.

## Building

The embedded IG client uses the `withoutebpf` build tag to exclude its Linux-only
host runtime. `make build`, `make test`, and distribution builds apply it. For
direct Go commands use `GOFLAGS=-tags=withoutebpf` (including `go build ./...`
and `go test ./internal/...`). Radar loads instrumentation only in the existing
Gadget daemon, never on the machine running Radar.

## Choosing evidence

Live Debug is a collapsed, opt-in section in Pod details. Use it for pinned
process/socket snapshots and short DNS windows on distroless workloads without
adding a container. Debug opens an interactive shell; Traffic shows the traffic
provider's retained or sampled flow evidence. These answer different questions.
Captures are unavailable through Radar Cloud's read-only connection.

DNS streams contain lifecycle state and filtered aggregate results, not raw
Gadget events. Peer enrichment lists at most 2,000 Pods and 2,000 Services with
the acting identity and a five-second budget; a namespace fallback or pagination
marks labels as partial. Process captures do not load those inventories. HTTP
DNS runs accept at most 60 seconds; the full MCP tool accepts at most 30 seconds.
The tool is excluded from read-only investigation endpoints and available only
on a confirmed write turn or through a write-capable MCP client.
