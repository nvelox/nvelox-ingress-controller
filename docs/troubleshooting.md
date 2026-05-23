# Troubleshooting

The same handful of failure modes account for most issues. Each section below: symptom → likely cause → confirm → fix.

## "I applied an Ingress and nothing happened"

**Cause 1: wrong IngressClass.** The controller only acts on Ingresses whose `spec.ingressClassName` matches the class it owns (default `nvelox`).

```bash
kubectl get ingress -A -o custom-columns='NAME:.metadata.name,CLASS:.spec.ingressClassName'
```

Set the right class or set `ingressClass.default=true` at install time and remove the explicit class from the Ingress.

**Cause 2: controller pod isn't running.**

```bash
kubectl -n nvelox-ingress get pods
kubectl -n nvelox-ingress describe pod -l app.kubernetes.io/name=nvelox-ingress-controller
```

Look for `ImagePullBackOff`, OOMKill, or CrashLoop. The most common boot-time failure is a wrong RBAC bundle — the controller will log `"forbidden: list ingresses"` in that case.

**Cause 3: numeric port required.** v1 only supports `backend.service.port.number`. If you used `backend.service.port.name`, the route gets dropped silently.

```bash
kubectl get ingress -A -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{": "}{range .spec.rules[*].http.paths[*]}{.backend.service.port}{" "}{end}{"\n"}{end}'
```

Anything showing `{name:foo}` won't render. Replace with `number:<num>`.

## "Route renders but I get 502 / connection refused"

The Ingress points at a Service that exists but has no Endpoints (no ready pods backing it), OR the Service port doesn't match the pod port.

```bash
kubectl get svc,endpoints <service-name>
kubectl describe svc <service-name>
```

The `Endpoints` row should list at least one `IP:port`. If empty, fix the Service `selector` or pod readiness.

## "TLS comes back with the wrong cert / self-signed warning"

**Cause 1: Secret has the wrong shape.** Must be `type: kubernetes.io/tls` with `data["tls.crt"]` and `data["tls.key"]`.

```bash
kubectl get secret <name> -o yaml | grep -E 'type:|tls\.(crt|key):'
```

**Cause 2: Secret is in a different namespace.** The controller materializes Secrets from the Ingress's own namespace only. Cross-namespace Secret refs aren't supported in `networking.k8s.io/v1`.

**Cause 3: cert-manager hasn't issued yet.**

```bash
kubectl get certificate -A
kubectl describe certificate <name>
```

`Ready: False` means the challenge is still in flight. Check `kubectl get challenges -A` and the cert-manager logs.

## "Config updates take a few seconds to apply"

That's the reconcile + reload cycle:

1. Informer cache sees the event (~100ms in steady state, more on first watch)
2. Reconciler runs (single-digit ms for small clusters)
3. Translator renders, hash compared, atomic write (<10ms)
4. SIGHUP → nvelox pre-flight validates → swap (<100ms)

Total typically under 500 ms. If you're seeing seconds, something is wrong:

* **WorkQueue backed up** — look at `workqueue_*` metrics on `:8082/metrics`
* **api-server slow** — `kubectl get --raw /metrics | grep request_duration_seconds`
* **Big Secret churn** — every Secret update across the cluster triggers a reconcile (cheap, but visible at scale); consider `kube-controller-manager` resync flags

## "Reload doesn't fire even though the Ingress changed"

**Cause: content-hash gate is doing its job.** The reconciler hashes the rendered output and skips the write+SIGHUP if it matches the last write. If your change is purely cosmetic (whitespace in an annotation we don't recognize, label change), the render is identical and we no-op on purpose.

Confirm with controller logs:

```bash
kubectl -n nvelox-ingress logs -l app.kubernetes.io/name=nvelox-ingress-controller \
  -c controller --tail=20
```

You'll see `"nvelox config updated"` only when the render actually changed.

To force a reload (debugging only):

```bash
kubectl -n nvelox-ingress exec deploy/nvelox-ingress-nvelox-ingress-controller -c nvelox -- kill -HUP 1
```

## "Controller logs 'nvelox process not found'"

Either nvelox hasn't started yet, OR `shareProcessNamespace` got disabled.

```bash
kubectl -n nvelox-ingress get pod -l app.kubernetes.io/name=nvelox-ingress-controller \
  -o jsonpath='{.items[0].spec.shareProcessNamespace}'
```

Must print `true`. If `<none>` or `false`, the chart was customized in a way that broke this. The Deployment template sets it; check your overlay values.

The first reconcile may also log this once during startup — the controller waits up to `--nvelox-wait` (30 s default) for nvelox to appear. After that window, the warning fires and the controller proceeds; nvelox will pick up the config on next reload.

## "Multiple replicas: only one is doing work"

That's leader election working correctly. Only the leader writes the file and signals nvelox; followers stay hot for failover.

```bash
kubectl -n nvelox-ingress get lease nvelox-ingress-controller -o yaml
```

`holderIdentity` shows the current leader pod.

## "Helm upgrade complains about resource ownership"

Usually leftover state from a previous `helm uninstall` that didn't clean cluster-scoped resources (IngressClass, ClusterRole). Fix:

```bash
make uninstall                          # cleans cluster-scoped leftovers
make install IMG=…                      # fresh install
```

If `make uninstall` still leaves something behind:

```bash
kubectl delete ingressclass nvelox
kubectl delete clusterrole,clusterrolebinding -l app.kubernetes.io/name=nvelox-ingress-controller
```

## Metrics worth watching

Controller metrics on `:8082/metrics`:

| Metric | What it tells you |
|---|---|
| `controller_runtime_reconcile_total{result="error"}` | Reconcile errors — usually api-server transient |
| `controller_runtime_reconcile_time_seconds` | Reconcile latency — should stay sub-second |
| `workqueue_depth` | Backlog — anything > 0 sustained is a sign of something looping |
| `workqueue_retries_total` | Retried items — pairs with reconcile errors above |

nvelox metrics on `:9090/metrics` (enabled by default in the chart): connection counts, request rates, backend health, TLS handshake timings.

Both are pre-annotated with `prometheus.io/scrape: true` on the pod so a standard kube-prometheus install picks them up.
