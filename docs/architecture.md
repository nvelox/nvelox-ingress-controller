# Architecture

```
┌─────────────────────────────────────────────────────────────────────┐
│ Pod  (shareProcessNamespace: true)                                  │
│                                                                     │
│  ┌─────────────────────────┐         ┌──────────────────────────┐   │
│  │ controller              │  writes │ nvelox                   │   │
│  │ (this binary)           │ ───────▶│ (data plane)             │   │
│  │                         │  SIGHUP │                          │   │
│  │ - controller-runtime    │ ◀──────▶│ binds :8080 / :8443      │   │
│  │   informers on:         │ (proc)  │ metrics :9090            │   │
│  │     Ingress             │         │ pid_file written to      │   │
│  │     Secret              │         │   /var/run/nvelox/       │   │
│  │ - render YAML           │         │                          │   │
│  │ - materialize tls.crt   │         │                          │   │
│  │ - locate pid + kill -1  │         │                          │   │
│  └─────────────────────────┘         └──────────────────────────┘   │
│                                                                     │
│  Shared volumes:                                                    │
│    /etc/nvelox/conf.d   (emptyDir) — rendered route fragment        │
│    /etc/nvelox/tls      (emptyDir) — materialized cert/key files    │
│    /var/run/nvelox      (emptyDir) — nvelox pid_file                │
│                                                                     │
└─────────────────────────────────────────────────────────────────────┘
            ▲
            │ Service (LoadBalancer/NodePort): :80 → :8080, :443 → :8443
            │
        Internet
```

## Why a sidecar instead of two Deployments

Reload trigger correctness. nvelox accepts hot reload via `SIGHUP`
(see `nvelox/README.md`). Sending a UNIX signal across pods would
require either:

* an HTTP reload endpoint on nvelox (changes nvelox), or
* a shared file watcher (changes nvelox), or
* `kubectl exec` from the controller into a nvelox pod (needs cluster
  API privileges + a long-lived exec channel — fragile, hard to RBAC)

`shareProcessNamespace: true` makes the sidecar pattern work without
modifying nvelox: the controller sees nvelox's PID in `/proc` (or
reads `pid_file` directly) and sends SIGHUP via `syscall.Kill`. Same
boundary, no IPC.

## Reconcile loop

1. Any `Ingress` or `Secret` event hits controller-runtime's workqueue.
2. The reconciler **rebuilds the entire config** rather than patching
   the previous render. Cost is bounded by total cluster ingress count
   (small); correctness wins (no "I deleted a route but the file still
   has it" bug).
3. The translator walks Ingresses → emits an in-memory
   `nveloxConfig{Listeners, Backends}`. Pure function — no I/O.
4. `sigs.k8s.io/yaml` serializes the struct. Output is **deterministic**:
   sorted lists, stable map iteration via key-sort.
5. The reloader hashes the output. **Identical → no-op.** This is
   the gate that lets us watch noisy resources (Secret churn from
   kube-controller-manager, resourceVersion bumps) without spamming
   nvelox with redundant SIGHUPs.
6. On a real change: atomic write (temp file + rename), then
   `syscall.Kill(pid, SIGHUP)`. nvelox's pre-flight validator either
   applies the new config atomically or rolls back — a malformed
   render here can't take the data plane down.

## PID discovery

Two paths, preferred order:

1. **`pid_file` in the shared `emptyDir`** (default
   `/var/run/nvelox/nvelox.pid`). Deterministic, single source of truth.
   nvelox writes this on startup; the controller reads it on every reload.
2. **`/proc/*/comm` fallback**. Used during the brief window after pod
   start where nvelox is alive but hasn't yet written `pid_file`. Walks
   `/proc`, matches against the configured `--proc-name`.

If both fail, `Apply` returns an error but **leaves the new config in
place** — the next natural reload (a real change, or a manual SIGHUP)
picks it up. We never roll back the file on a signal failure, because
the file represents the latest desired state.

## Translator output shape

The rendered YAML is purely additive to the base config — it's
`include`-d via the operator-provided base ConfigMap:

```yaml
# /etc/nvelox/nvelox.yaml (ConfigMap)
version: "2"
server: { pid_file: /var/run/nvelox/nvelox.pid }
logging: { level: info }
include: /etc/nvelox/conf.d/*.yaml      # ← our render lands here
```

```yaml
# /etc/nvelox/conf.d/k8s.yaml (controller output)
listeners:
  - name: k8s-http
    bind: ":8080"
    protocol: http
    routes:
      - match: { host: echo.example.com, path_prefix: / }
        backend: k8s-default-echo-5678
  - name: https-default-echo-secure-example-com
    bind: ":8443"
    protocol: https
    server_names: [secure.example.com]
    tls:
      cert_file: /etc/nvelox/tls/default-echo-tls.crt
      key_file:  /etc/nvelox/tls/default-echo-tls.key
    routes:
      - match: { host: secure.example.com, path_prefix: / }
        backend: k8s-default-echo-5678
backends:
  - name: k8s-default-echo-5678
    servers:
      - addr: echo.default.svc.cluster.local:5678
```

## What lives where

| Path inside the container                | Producer    | Consumer | Purpose                                |
|---|---|---|---|
| `/etc/nvelox/nvelox.yaml`                | Helm ConfigMap | nvelox  | Base config (logging, admin, includes) |
| `/etc/nvelox/conf.d/k8s.yaml`            | controller  | nvelox   | Rendered ingress routes + backends     |
| `/etc/nvelox/tls/<ns>-<name>.crt/.key`   | controller  | nvelox   | Materialized cert/key from Secrets     |
| `/var/run/nvelox/nvelox.pid`             | nvelox      | controller | PID file for SIGHUP target locator   |

## Failure modes

| What fails                                | Behavior                                              |
|---|---|
| api-server unreachable                     | controller-runtime retries with backoff; nvelox unaffected |
| Secret referenced by `spec.tls` is missing | translator skips that listener; eventual Secret create event re-fires reconcile and materializes it |
| File write fails (disk full, RO mount)     | reconcile returns error → re-queued with backoff; previous file + previous nvelox state preserved |
| SIGHUP fails (pid_file empty + /proc miss) | file write succeeded; logs WARN; nvelox picks up on next natural reload OR pod restart |
| nvelox rejects the new config (invalid)    | nvelox's own pre-flight rolls back; data plane keeps serving the previous config |
| Two replicas race                          | Enable `leaderElection.enabled=true`; only the leader writes |

## Pod IP changes (EndpointSlices mode)

When backends are resolved via EndpointSlices (`#210`, default behavior), nvelox holds pod IPs directly instead of Service VIPs. This unlocks nvelox's L4 strategies (least-conn, sticky-IP-hash, per-pod cookies) but means pod IP changes need to propagate through the controller.

**Steady-state flow on pod replacement:**

1. Pod terminating → readiness probe fails → kubelet flips the EndpointSlice entry to `Ready=false`
2. EndpointSlice watch fires reconcile → `buildEndpointAddressMap` filters out NotReady endpoints → translator re-renders without that pod IP
3. Reloader writes + SIGHUPs nvelox → atomic backend swap
4. **Then** kubelet sends SIGTERM and the pod dies

So a graceful shutdown is invisible to clients: the pod stops receiving new traffic from nvelox before its process exits. The order is enforced by the EndpointSlice readiness flip happening on probe failure, not on container exit.

**Where it CAN go wrong:**

* Pod crashes mid-flight (SIGSEGV, OOMKill) — readiness probe doesn't get a chance to flip first. EndpointSlice catches up via the next kubelet sync (~10 s default). During that window, nvelox sends requests to the dead IP → connection refused/timeout → bare 502s.
* Pod gets a new IP on restart and the EndpointSlice update reaches the controller before the pod is actually serving on its new IP. nvelox forwards to the new IP, pod refuses, brief 502s until pod is up.

**Operator mitigations for HA-critical paths:**

| Mitigation | What it buys you |
|---|---|
| `preStop` hook + `terminationGracePeriodSeconds: 30s` | Pod drains in-flight conns + stops accepting new ones before exit; EndpointSlice flips before SIGTERM lands |
| `PodDisruptionBudget` with `maxUnavailable: 1` | Bounds concurrent pod replacements so the backend stays warm during rollouts |
| nvelox passive health checks in base config | nvelox marks a backend dead on connection failure; subsequent requests skip it without waiting for the EndpointSlice update |
| `livenessProbe` distinct from `readinessProbe` | Lets a struggling pod drop out of routing without being killed — gives the controller time to update |

**Falling back to kube-proxy if you want simpler semantics:**

The translator falls back to the Service-DNS form when EndpointSlices yields zero entries for a backend. To force fallback cluster-wide, point the controller at a build that never populates `EndpointAddresses` — pragmatically that means commenting out the `buildEndpointAddressMap` call in the reconciler. Worth doing if your workload's pod-restart blast radius is unacceptable AND you don't need the L4 strategies. Most users won't need this.
