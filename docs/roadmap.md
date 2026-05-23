# Roadmap

What's in v1, what's queued, and what's deliberately deferred. Issues / discussion belong on the project tracker; this is just the working backlog.

## v1 — shipped

* Watch `Ingress` (filtered by IngressClass) + `Secret` (for TLS)
* Translate to nvelox YAML (host + path-prefix routing, HTTPS termination)
* Atomic write + content-hash gate + SIGHUP-based reload
* Helm chart with controller + nvelox sidecar in one Pod, shared volumes
* Leader election (opt-in)
* `IngressClass` registration with `ingressclass.kubernetes.io/is-default-class` support
* Samples covering the common patterns (HTTP, TLS, cert-manager, fanout, multi-host, catch-all)

## v1.x — next, in order

### 1. Translator unit tests
Golden-file approach: one fixture per Ingress shape (single host, multi-host, TLS, path fanout, catch-all). Diff the rendered YAML against the expected bytes. Cheap regression net; prerequisite for any further translator changes. **SHIPPED** (`internal/translator/translator_test.go`).

### 1a. Schema-validity gate for translator output — PRIORITY: HIGH
The byte-level golden tests catch regressions but can't tell you the golden was WRONG IN THE FIRST PLACE. Track record so far: **two shape regressions shipped, both caught only by smoke**:

* `backends.servers` — emitted as `[]struct{Addr}` (map), nvelox wants `[]string`. Caught after first real run.
* `tls.cert_file`/`key_file` — wrong field names; nvelox wants `cert`/`key`. Caught after extending smoke for #202.

Both would have been caught at `go test` time by a schema gate. Two complementary guards:

* **CI smoke** (`.github/workflows/ci.yml`) — gates PRs end-to-end. **SHIPPED**.
* **`nvelox validate <file>`** invoked from the test suite — needs an upstream issue / PR against nvelox to add the subcommand if not already present. Once available, the test suite rejects shape regressions in milliseconds instead of waiting for a kind cluster to come up. **Pending** (#212).

Until #212 lands: every translator change MUST go through smoke before merging.

### 2. Named service ports — SHIPPED
Resolves `backend.service.port.name` against the target Service's `spec.ports`. Controller watches Services (port-name renames re-fire reconcile) and hands a `<ns>/<svc>/<portName> → portNumber` map to the translator. Missing Service / missing port name → route dropped fail-closed.

### 3. Ingress status updates — SHIPPED
Reconciler now publishes the fronting Service's external address back to every owned Ingress's `status.loadBalancer.ingress[]`. Discovery rules: LoadBalancer → `.status.loadBalancer.ingress`; NodePort → Node InternalIPs; ClusterIP → no status updates. Wired via `--publish-service=<ns>/<name>` flag, defaulted in the chart to the chart's own Service. Toggle off with `publishStatus.enabled=false` in values.

### 4. Annotation set
Patterned on Traefik / ingress-nginx. Each annotation gets a struct field in `internal/annotations`, a translator hook, a unit test, a doc row, and a sample. Infrastructure shared across them: `annotations.Parse(*Ingress) Spec`.

| Annotation | Maps to nvelox | Status |
|---|---|---|
| `nvelox.io/redirect-https`            | route `redirect` (301 to https://) | **SHIPPED** |
| `nvelox.io/rate-limit-per-second`     | listener `ip_rate_limit` | **SHIPPED** (combinable with per-minute) |
| `nvelox.io/rate-limit-per-minute`     | listener `ip_rate_limit` (ceil/60 → RPS) | **SHIPPED** |
| `nvelox.io/sticky-cookie`             | backend `sticky_session.cookie` | **SHIPPED** (full effect needs #210) |
| `nvelox.io/allow-cidrs`               | listener `ip_allowlist` | **SHIPPED** |
| `nvelox.io/deny-cidrs`                | listener `ip_denylist` | **SHIPPED** |
| `nvelox.io/strip-prefix`              | route `rewrite` rule | **SHIPPED** |
| `nvelox.io/request-headers`           | route `headers.request_add` | **SHIPPED** |
| `nvelox.io/response-headers`          | route `headers.response_add` | **SHIPPED** |

Each annotation needs documented precedence (Ingress > IngressClass parameters > defaults) and a clear failure mode for invalid values (skip + log + emit Event, never block reconcile).

### 5. Stale-TLS-file GC — SHIPPED
Reconciler tracks the keep-set (basenames `<ns>-<name>.crt` / `.key`) per reconcile and prunes anything else under `TLSCertDir` matching that naming. Prune happens AFTER the writes — a rotation that replaces a Secret with a different name never has a window where neither file exists.

### 6. EndpointSlices instead of Service DNS — SHIPPED
Controller watches `discovery.k8s.io/v1` EndpointSlices and emits one `backend.servers` entry per Ready pod IP. Falls back to the Service-DNS form when no endpoints are visible (pre-Ready Services, or installs that skip the slice informer). Unlocks nvelox's L4 strategies and is the prerequisite that activates #204's per-pod sticky cookie at runtime.

## v2 — bigger pieces

### IngressClass parameters — SHIPPED (ConfigMap form)
`IngressClass.spec.parameters` pointing at a ConfigMap is now honored. ConfigMap keys carry the same `nvelox.io/*` annotation values; bare keys (no prefix) also accepted. The controller fetches the parameters once per reconcile and merges them UNDER per-Ingress annotations via `annotations.Merge` — scalar fields the Ingress sets win, CIDR lists UNION, header maps merge per-key. Sample at `samples/13-class-defaults.yaml`.

**Follow-up — typed CRD form:** the ConfigMap path covers every knob we'd put in a typed CRD today and avoids the code-generation tooling. Switching to a `NveloxIngressClassConfig` CRD later only changes the parameters-resolution function (kind switch in `loadClassDefaults`) — the merge layer doesn't need to change. Worth doing when (a) we need fields that don't fit string-keyed values (e.g., complex types like timeout durations with units validation), or (b) operators ask for kubectl-explain support on the parameters shape.

**Known limitation:** `nvelox.io/redirect-https` (boolean) defaults aren't supported yet because our parser can't distinguish "annotation absent" from "annotation=false". Adding presence tracking is a one-day cleanup if anyone needs it.

### `NveloxRoute` CRD (Traefik IngressRoute analog) — DESIGN BELOW, IMPLEMENTATION DEFERRED

Why not shipped in this batch: even a tight cut is multi-day work and warrants its own session. Design sketched so the next session can start coding instead of re-designing.

**Scope cut for v1 of the CRD:**

* **TCP + TLS-passthrough routes only.** HTTP already works fine via `networking.k8s.io/v1` Ingress + annotations; the value of NveloxRoute is unlocking nvelox features the Ingress spec can't express. TCP / TLS-passthrough is the most-requested gap.
* **No middleware chains, no multi-match conditions.** Keep the spec flat. Middleware (rate-limit, sticky, ACL) keeps the annotation form on the parallel Ingress flow until we have data showing operators want chainable middleware. YAGNI gate.
* **Direct selector, no parentRef.** A `NveloxRoute` carries its own listener config. One CRD = one nvelox listener. Gateway-style parent refs come with Gateway API support (below).

**Skeleton CRD shape:**

```yaml
apiVersion: nvelox.io/v1alpha1
kind: NveloxRoute
metadata:
  name: postgres
spec:
  type: tcp                          # tcp | tls-passthrough
  bind: ":5432"                      # in-pod port nvelox binds
  tls:                                # only for tls-passthrough
    serverNames: ["db.example.com"]
  backendRef:
    name: postgres-primary
    namespace: db
    port: 5432
```

**Implementation hits, in order:**

1. `api/v1alpha1/` package — Go types + `+kubebuilder:` markers. Pull `controller-gen` for `zz_generated.deepcopy.go`.
2. `config/crd/` — generated CRD YAML; chart bundles into `crds/`.
3. New reconciler `internal/nveloxroute/reconciler.go` — watches the CRD, emits per-route listener blocks into `/etc/nvelox/conf.d/routes.yaml`.
4. Status subresource — `Accepted` / `ResolvedRefs` conditions per route. Mirrors Gateway API patterns.
5. Smoke test — apply a TCP NveloxRoute, run `nc` against the in-pod TCP port, assert backend got the connection.

**Estimated effort:** 1-2 focused sessions for TCP scope. Each additional protocol (UDP, gRPC, HTTP/3) is its own session — grows the schema + smoke story, reuses the reconciler scaffolding.

### Gateway API support (HTTPRoute / TCPRoute / TLSRoute) — DEFERRED

Why deferred: Gateway API is the long-term direction but adds substantial dependency weight (`sigs.k8s.io/gateway-api`) and forces a parallel implementation that has to stay in sync with the Ingress + NveloxRoute paths. Not worth shipping until NveloxRoute (above) has proven the route-translation pattern.

**When to revisit:** after NveloxRoute v1 ships and at least one production user runs it.

1. Add `sigs.k8s.io/gateway-api` to go.mod.
2. New reconcilers for `Gateway`, `HTTPRoute`, `TCPRoute`, `TLSRoute` — each translates into the same nvelox listener/route emission.
3. Status writes for every Route kind + parentRef. Biggest cost — Gateway API status is rich.
4. `GatewayClass` registration with controller name `nvelox.io/gateway`.
5. Conformance suite — Gateway API's conformance tests are non-optional for credibility.

**Estimated effort:** 1-2 weeks. Don't promise it until you have real demand.

### Multi-cluster fanout — DESIGN OPEN, NO IMPLEMENTATION

Two viable shapes; the choice depends on operator topology, which we don't have data on yet:

**Shape A — Federated nvelox fleet.** One pool of nvelox pods sits ABOVE the clusters. Pod IP discovery happens cross-cluster via a service mesh handoff (Istio, Linkerd) or an EndpointSlice-mirror like Submariner provides.

* Pro: one config plane, one cert plane, single point of policy.
* Con: requires a mesh; cross-cluster pod IP routing is gnarly; LB blast radius is the whole fleet.

**Shape B — Cluster-local nvelox, federated control plane.** Each cluster runs its own nvelox + this controller; a SEPARATE binary (call it `nvelox-fed`) watches Ingresses across all clusters and pushes a merged config to each.

* Pro: no mesh dependency, clusters stay autonomous, blast radius limited to one cluster.
* Con: another binary to operate, federation control-plane becomes a SPOF unless HA'd.

**What's needed to make a choice:** at least one production user with a concrete multi-cluster need. Until then, no design lock-in.

## Explicitly out of scope

* **Built-in dashboard** — nvelox already ships an admin API; if a UI is wanted, build it on top of that API, not inside this controller.
* **Mesh sidecar mode** — not the right shape for nvelox; if you want per-pod proxies, use a mesh.
* **Cert issuance** — cert-manager already does this well; we consume the Secrets it produces.

## How to propose changes

1. File an issue with the use case (not the proposed implementation).
2. Discussion lands on the desired UX shape (Ingress vs annotation vs CRD).
3. Smallest-cut PR that lands the feature behind a default-off chart value.
4. Promote to default-on once at least one production user reports it stable.
