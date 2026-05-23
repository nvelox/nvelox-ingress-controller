# nvelox-ingress samples

Applyable example manifests covering the patterns the controller
supports today. Each file is self-contained — apply directly with
`kubectl apply -f samples/<file>.yaml`.

| File | What it shows |
|---|---|
| [01-basic-http.yaml](01-basic-http.yaml)            | Smallest useful HTTP Ingress — Deployment + Service + Ingress, single host |
| [02-tls-secret.yaml](02-tls-secret.yaml)            | HTTPS termination with a hand-managed `kubernetes.io/tls` Secret |
| [03-tls-cert-manager.yaml](03-tls-cert-manager.yaml) | HTTPS with auto-issued certs via cert-manager + Let's Encrypt |
| [04-path-fanout.yaml](04-path-fanout.yaml)          | One host, multiple paths, each routed to a different Service |
| [05-multi-host.yaml](05-multi-host.yaml)            | One Ingress, multiple hosts, each routed independently |
| [06-default-backend.yaml](06-default-backend.yaml)  | Wildcard catch-all for requests that don't match a specific host |
| [07-redirect-https.yaml](07-redirect-https.yaml)    | `nvelox.io/redirect-https: "true"` — every HTTP request 301s to HTTPS |
| [08-rate-limit.yaml](08-rate-limit.yaml)            | `nvelox.io/rate-limit-per-{second,minute}` — per-client-IP throttling |
| [09-sticky-cookie.yaml](09-sticky-cookie.yaml)      | `nvelox.io/sticky-cookie` — cookie-based session affinity (full effect needs #210) |
| [10-cidr-acls.yaml](10-cidr-acls.yaml)              | `nvelox.io/allow-cidrs` / `deny-cidrs` — per-listener CIDR ACLs |
| [11-strip-prefix.yaml](11-strip-prefix.yaml)        | `nvelox.io/strip-prefix` — drop a URL prefix before forwarding |
| [12-headers.yaml](12-headers.yaml)                  | `nvelox.io/request-headers` / `response-headers` — per-route header injection |
| [13-class-defaults.yaml](13-class-defaults.yaml)    | `IngressClass.spec.parameters` → ConfigMap → cluster-wide annotation defaults |
| [14-named-port.yaml](14-named-port.yaml)            | Backend referenced by `port.name` instead of `port.number` (named-port resolution) |

## Quick smoke test

```bash
# 1. Install the controller
make install IMG=ghcr.io/nvelox/nvelox-ingress-controller:dev

# 2. Apply the basic sample
make install-sample      # equivalent to: kubectl apply -f samples/01-basic-http.yaml

# 3. Watch the controller pick it up
kubectl -n nvelox-ingress logs -l app.kubernetes.io/name=nvelox-ingress-controller \
  -c controller --tail=20 -f

# 4. Hit the route
kubectl -n nvelox-ingress port-forward svc/nvelox-ingress 8080:80 &
curl -H 'Host: echo.example.com' http://localhost:8080/
# → hello-from-nvelox
```

## Not in this directory yet

* **TCP / UDP / TLS-passthrough ingress** — needs the `NveloxRoute`
  CRD (scoped design in `docs/roadmap.md`, implementation deferred).
* **Per-route rate-limit / ACL** — today these are per-listener;
  per-route requires nvelox to grow route-level `rate_limit` first.
  Current samples (08, 10) work at listener scope, which isolates
  per-HTTPS-host but shares across the HTTP catch-all listener.
* **Gateway API** (`HTTPRoute` / `TCPRoute` / `TLSRoute`) —
  deferred until `NveloxRoute` ships and at least one operator
  needs Gateway API compatibility.
