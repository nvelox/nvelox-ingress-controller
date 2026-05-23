# nvelox-ingress-controller

Kubernetes Ingress controller that drives an [nvelox](https://github.com/nvelox/nvelox) sidecar.

```
┌─────────────────────────────────────────────────────────────────────┐
│ Pod  (shareProcessNamespace: true)                                  │
│                                                                     │
│  ┌─────────────────────────┐         ┌──────────────────────────┐   │
│  │ controller              │  writes │ nvelox                   │   │
│  │ (this binary)           │ ───────▶│ (data plane)             │   │
│  │                         │  SIGHUP │                          │   │
│  │ watches:                │ ◀──────▶│ binds :8080 / :8443      │   │
│  │   Ingress, IngressClass │ (proc)  │ metrics :9090            │   │
│  │   Secret                │         │ pid_file → /var/run/nvelox/ │
│  │   Service               │         │                          │   │
│  │   EndpointSlice         │         │                          │   │
│  │   ConfigMap (params)    │         │                          │   │
│  └─────────────────────────┘         └──────────────────────────┘   │
│                                                                     │
│  Shared volumes:                                                    │
│    /etc/nvelox/conf.d       — rendered route fragment               │
│    /etc/nvelox/tls          — materialized cert/key files           │
│    /etc/nvelox/default-www  — empty static-root for the catch-all   │
│    /var/run/nvelox          — pid_file                              │
└─────────────────────────────────────────────────────────────────────┘
            ▲
            │ Service (LoadBalancer/NodePort): :80 → :8080, :443 → :8443
            │
        Internet
```

## What it does

* Watches `Ingress` resources whose `spec.ingressClassName` matches the configured class (default `nvelox`).
* Watches every `Secret` referenced by `spec.tls` — cert-manager renewal re-renders without manual intervention.
* Watches `Service` + `EndpointSlice` so backend changes (port renames, pod IPs flipping, readiness changes) propagate in seconds.
* Translates the union into nvelox YAML under `/etc/nvelox/conf.d/k8s.yaml`.
* Sends `SIGHUP` to the nvelox PID in the shared process namespace — nvelox's pre-flight validator either applies atomically or rolls back.
* Materializes referenced TLS Secrets into the shared `tls` volume + GCs stale files when references go away.
* Publishes the fronting Service's external address back to every owned Ingress's `status.loadBalancer.ingress[]`.

## Features

| What | How | Sample |
|---|---|---|
| HTTP / HTTPS routing by host + path-prefix | Plain `networking.k8s.io/v1` Ingress | [01-basic-http.yaml](samples/01-basic-http.yaml) |
| TLS termination at nvelox | `spec.tls[]` with `kubernetes.io/tls` Secret | [02](samples/02-tls-secret.yaml), [cert-manager](samples/03-tls-cert-manager.yaml) |
| Named service ports | `backend.service.port.name` | [14-named-port.yaml](samples/14-named-port.yaml) |
| Default backend (catch-all 404) | always-on listener; configurable | [06-default-backend.yaml](samples/06-default-backend.yaml) |
| HTTP→HTTPS redirect | `nvelox.io/redirect-https: "true"` | [07-redirect-https.yaml](samples/07-redirect-https.yaml) |
| Per-IP rate limit (per-sec / per-min / both) | `nvelox.io/rate-limit-per-{second,minute}` | [08-rate-limit.yaml](samples/08-rate-limit.yaml) |
| Cookie-based session affinity | `nvelox.io/sticky-cookie` | [09-sticky-cookie.yaml](samples/09-sticky-cookie.yaml) |
| Per-listener CIDR allow / deny | `nvelox.io/allow-cidrs`, `nvelox.io/deny-cidrs` | [10-cidr-acls.yaml](samples/10-cidr-acls.yaml) |
| Strip URL prefix before forwarding | `nvelox.io/strip-prefix` | [11-strip-prefix.yaml](samples/11-strip-prefix.yaml) |
| Request / response header injection | `nvelox.io/{request,response}-headers` | [12-headers.yaml](samples/12-headers.yaml) |
| Cluster-wide annotation defaults | `IngressClass.spec.parameters` → ConfigMap | [13-class-defaults.yaml](samples/13-class-defaults.yaml) |
| Per-pod direct routing (bypass kube-proxy) | EndpointSlices watch (automatic) | n/a — transparent |
| `Ingress.status.loadBalancer` publishing | Service-IP discovery (automatic) | n/a — transparent |
| TLS file GC | reconciler keep-set after every reload | n/a — transparent |

Full field-by-field mapping in [`docs/ingress-mapping.md`](docs/ingress-mapping.md).

## Install

```bash
make install \
  KUBECONFIG=/path/to/kubeconfig \
  IMG=ghcr.io/nvelox/nvelox-ingress-controller:v0.1.0
```

For a private registry:

```bash
make install \
  KUBECONFIG=/path/to/kubeconfig \
  IMG=registry.example.com/nvelox/nvelox-ingress-controller:dev \
  NVELOX_IMG=registry.example.com/nvelox/nvelox:dev
```

Knobs: `INGRESS_CLASS`, `DEFAULT_CLASS`, `SERVICE_TYPE`, `NAMESPACE`, `RELEASE`, `VALUES_FILE` — see [`docs/install.md`](docs/install.md) for the full table + chart overlay form.

## Apply a sample

```bash
make install-sample      # applies samples/01-basic-http.yaml
```

## End-to-end smoke test

```bash
# Against your current kubectl context:
make smoke IMG=registry.example.com/nvelox/nvelox-ingress-controller:dev PUSH=1

# Against a throwaway kind cluster:
make smoke USE_KIND=1
```

Covers eleven feature paths end-to-end (basic + every annotation + status + EndpointSlices + IngressClass parameters). Pass/fail = exit code.

## Uninstall

```bash
make uninstall KUBECONFIG=/path/to/kubeconfig
```

Idempotent: removes the Helm release + cluster-scoped IngressClass / RBAC. Leaves the namespace intact.

## Docs

| | |
|---|---|
| [docs/install.md](docs/install.md)                    | Install + upgrade + smoke flows |
| [docs/architecture.md](docs/architecture.md)          | How the controller + sidecar fit, reload mechanism, every failure mode |
| [docs/ingress-mapping.md](docs/ingress-mapping.md)    | What every Ingress field + annotation becomes in nvelox YAML |
| [docs/troubleshooting.md](docs/troubleshooting.md)    | Symptom → cause → fix for common breakages |
| [docs/roadmap.md](docs/roadmap.md)                    | What's shipped, deferred, and out-of-scope |

## Building locally

```bash
make build                                          # Go binary → bin/
make test                                           # go test ./...
make docker-build IMG=registry.example.com/x:dev    # docker build
make kind-load    IMG=registry.example.com/x:dev    # side-load to kind
make helm-lint                                      # lint the chart
make helm-template                                  # see what helm would render
```

## License + status

See `docs/roadmap.md`. v1.x is feature-complete; v2 items (NveloxRoute CRD, Gateway API, multi-cluster) are scoped with implementation plans but deferred until a focused session per item.
