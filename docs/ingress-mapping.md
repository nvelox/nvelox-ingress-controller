# Ingress field mapping

Reference for what every `networking.k8s.io/v1` Ingress field becomes in nvelox config. **v1 cut** — features marked "not yet" are deliberate scope-cuts tracked in `roadmap.md`.

## `spec.ingressClassName`

Must match the configured class (default `nvelox`). The controller silently ignores Ingresses with a different class or no class at all.

Set `ingressClass.default=true` in values to make `nvelox` the cluster default — then Ingresses without an explicit `ingressClassName` get adopted.

## `spec.rules[]`

| Field                                         | Rendered as                                                                       |
|---|---|
| `rules[].host`                                | HTTP: `route.match.host`. HTTPS: listener's `server_names: [host]` (SNI dispatch) |
| `rules[].host` empty                          | Catch-all route on the HTTP listener (matches any Host)                           |
| `rules[].http.paths[].path`                   | `route.match.path_prefix`                                                         |
| `rules[].http.paths[].pathType: Prefix`       | `path_prefix` match                                                               |
| `rules[].http.paths[].pathType: Exact`        | **Approximated as Prefix** (nvelox doesn't have a dedicated exact matcher yet)    |
| `rules[].http.paths[].pathType: ImplementationSpecific` | `path_prefix` match                                                     |
| `rules[].http.paths[].backend.service.name`   | Component of the backend address: `<name>.<ns>.svc.cluster.local`                 |
| `rules[].http.paths[].backend.service.port.number` | Component of the backend address: `:<port>`                                   |
| `rules[].http.paths[].backend.service.port.name`   | Resolved against the target Service's `spec.ports[name=X].port`. Missing Service or missing port name → route is dropped silently (fail-closed; the next Service event re-fires reconcile and the route re-renders) |
| `rules[].http.paths[].backend.resource`       | **Not yet** — provider-specific resource backends                                 |

Routes inside a host preserve the order you write them. nvelox is **first-match-wins**, so put specific paths before catch-alls.

## `spec.tls[]`

| Field                                         | Rendered as                                                                       |
|---|---|
| `tls[].hosts`                                 | Each host gets its own HTTPS listener entry (sharing `:8443`, dispatched via SNI) |
| `tls[].secretName`                            | Controller reads `Secret.data["tls.crt"]` + `["tls.key"]`, writes them to `/etc/nvelox/tls/<ns>-<name>.crt/.key`, references those paths from `listener.tls.cert_file` / `key_file` |
| Secret missing                                | Listener is **skipped** until the Secret is created (next Secret event re-fires the reconcile) |
| Secret type ≠ `kubernetes.io/tls`             | Treated like a missing Secret if `tls.crt`/`tls.key` keys aren't present          |

## `spec.defaultBackend`

**Not yet.** Use a rule with no `host:` instead — see `samples/06-default-backend.yaml`.

## Annotations

All annotations use the `nvelox.io/` prefix. Invalid values are logged + skipped (never block reconcile); absence = default behavior. Precedence is annotation > IngressClass parameters > built-in default.

| Annotation                       | Type | Value           | What it does                                                                                                  |
|---|---|---|---|
| `nvelox.io/redirect-https`       | bool | `"true"` / `"false"` | When true, emits a 301 redirect on the HTTP listener for every host in `spec.tls`. HTTPS listener still serves the real backend. No-op for hosts without TLS. |
| `nvelox.io/rate-limit-per-second`| int  | positive integer     | Per-client-IP token-bucket limit. Maps to nvelox's listener-level `ip_rate_limit.requests_per_second`; burst defaults to the same value. |
| `nvelox.io/rate-limit-per-minute`| int  | positive integer     | Same primitive as per-second but in /min terms. Converted via `ceil(N/60)` → RPS; burst = N. Combinable with per-second: more restrictive wins. |
| `nvelox.io/sticky-cookie`        | string | cookie name        | Cookie-based session affinity on the backend(s) this Ingress references. Emits nvelox's `sticky_session{type=cookie, cookie_name=<value>, ttl=1h}`. First-Ingress-wins when multiple Ingresses reference the same backend. With per-pod routing (EndpointSlices) live, the cookie now pins to a specific pod as expected. |
| `nvelox.io/allow-cidrs`          | csv  | comma-separated CIDR list | Per-listener `ip_allowlist`. When set, ONLY listed CIDRs reach the listener. Invalid CIDR entries are dropped + logged (rest of the list still applies). |
| `nvelox.io/deny-cidrs`           | csv  | comma-separated CIDR list | Per-listener `ip_denylist`. Listed CIDRs are blocked at L3. Stacks with allow-cidrs: allowlist first, then denylist. |
| `nvelox.io/strip-prefix`         | string | absolute path           | Drops this prefix from every request URI on routes this Ingress emits. Switches the route's match from `path_prefix` to `path_regex` with capture; emits `rewrite.path`. Silently no-op for routes whose path doesn't start with the strip-prefix. |
| `nvelox.io/request-headers`      | multi-line | "Name: Value" per line | Per-route `headers.request_add` — added to backend-bound requests. Bad lines dropped + logged. |
| `nvelox.io/response-headers`     | multi-line | "Name: Value" per line | Per-route `headers.response_add` — added to client-bound responses. Bad lines dropped + logged. |

**CIDR scope (important):** `ip_allowlist` / `ip_denylist` are per-listener. HTTPS host = own listener = isolated filters. The shared `k8s-http` listener UNIONs both lists across contributing Ingresses; deny is safely additive but a strict allow-cidrs on one HTTP Ingress restricts every neighbour on the shared listener too. Route the strict-allowlist host through HTTPS for true isolation.

**Rate-limit scope (important):** `ip_rate_limit` is per-listener in nvelox, not per-route. In the rendered config each HTTPS host has its own listener — annotation isolates per-host. The shared `k8s-http` listener gets the **most restrictive** annotation across all HTTP Ingresses that contribute to it. Per-route rate-limit comes when nvelox grows route-level `rate_limit`.

## `IngressClass.spec.parameters`

The controller honors `parameters` that point at a **ConfigMap** in the cluster. ConfigMap keys carry the same `nvelox.io/*` annotation names (bare-key form without the prefix also accepted). Values become cluster-wide defaults; per-Ingress annotations always win for the same field. Sample: [`samples/13-class-defaults.yaml`](../samples/13-class-defaults.yaml).

Merge semantics:

| Field type | Merge rule |
|---|---|
| Scalar (rate-limit-*, sticky-cookie, strip-prefix) | Non-zero / non-empty per-Ingress wins; else default applies |
| CIDR list (allow-cidrs, deny-cidrs)                | UNION of default + per-Ingress, deduped |
| Header map (request-headers, response-headers)     | Per-key override; per-Ingress wins for same key, else default |
| Bool (redirect-https)                              | Not supported in defaults yet (parser can't distinguish "absent" from "false") |

Custom CRD targets aren't supported yet — track the planned `NveloxIngressClassConfig` CRD in [`roadmap.md`](roadmap.md).

## Service discovery

Backends resolve via **`discovery.k8s.io/v1` EndpointSlices**: one `backend.servers` entry per Ready pod IP, bypassing kube-proxy. Falls back to the Service-DNS form (`<svc>.<ns>.svc.cluster.local:<port>`) when no Ready endpoints are visible — covers pre-Ready Services, deployments without EndpointSlices, and the brief window during pod IP changes.

NotReady endpoints (failing probe, terminating pods) are filtered out before reaching nvelox, so a graceful pod shutdown flips the EndpointSlice to NotReady → controller re-renders → nvelox stops sending → THEN the pod terminates. See [`architecture.md`](architecture.md#pod-ip-changes-endpointslices-mode) for the full HA story + mitigations.

## Status updates

`Ingress.status.loadBalancer.ingress[]` is populated automatically from the controller's fronting Service. Discovery rules:

| Service type    | What gets published                                         |
|---|---|
| `LoadBalancer`  | `Service.status.loadBalancer.ingress[]` (IPs + hostnames)   |
| `NodePort`      | Cluster's Node InternalIPs                                  |
| `ClusterIP`     | Nothing (no externally-reachable address — typical when fronted by something else; misleading to publish) |

Toggle off with `publishStatus.enabled=false` in the Helm chart values if Ingress status is managed elsewhere (e.g., external-dns).
