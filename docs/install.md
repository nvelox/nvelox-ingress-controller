# Install

## Prerequisites

* Kubernetes **1.27+** (uses `networking.k8s.io/v1` Ingress, `IngressClass`)
* Helm **3.8+**
* A container registry the cluster can pull from, OR `kind`/`minikube` and the controller image side-loaded

## Helm install (recommended)

```bash
make install \
  KUBECONFIG=/path/to/kubeconfig \
  IMG=ghcr.io/nvelox/nvelox-ingress-controller:v0.1.0
```

What it does:

1. Creates the `nvelox-ingress` namespace (idempotent)
2. Annotates/labels it so Helm owns it cleanly
3. Runs `helm upgrade --install` against `deploy/helm/nvelox-ingress-controller/`
4. Waits up to 120 s for the Deployment to roll out

Result:

* One pod (`controller` + `nvelox` sidecar)
* One Service (`LoadBalancer` by default, mapping `:80 → :8080` and `:443 → :8443`)
* One `IngressClass` named `nvelox` (controller field: `nvelox.io/ingress`)
* `ClusterRole` + `ClusterRoleBinding` granting `get/list/watch` on Ingresses, Services, Secrets

## Make-time knobs

```bash
make install IMG=…                                    # required
                INGRESS_CLASS=ngris-prod              # override class name
                DEFAULT_CLASS=true                    # mark cluster-default
                SERVICE_TYPE=NodePort                 # for bare-metal / kind
                NVELOX_IMG=ghcr.io/nvelox/nvelox:0.5  # pin sidecar version
                VALUES_FILE=overlay.yaml              # extra helm --values
                NAMESPACE=ingress-system              # custom namespace
                RELEASE=primary                       # custom release name
```

## Raw `helm install` if you don't want the Makefile

```bash
helm upgrade --install nvelox-ingress \
  ./deploy/helm/nvelox-ingress-controller \
  --namespace nvelox-ingress --create-namespace \
  --set controller.image.repository=ghcr.io/nvelox/nvelox-ingress-controller \
  --set controller.image.tag=v0.1.0 \
  --set service.type=LoadBalancer
```

## Verify

```bash
kubectl -n nvelox-ingress get pods,svc,ingressclass
kubectl -n nvelox-ingress logs -l app.kubernetes.io/name=nvelox-ingress-controller \
  -c controller --tail=20
```

You should see:

```
"msg":"nvelox-ingress-controller starting" "ingress_class":"nvelox" ...
"msg":"nvelox config updated" "ingresses":0
```

(The "ingresses: 0" line means the controller started clean and is ready to pick up Ingress events.)

## Uninstall

```bash
make uninstall KUBECONFIG=…
```

What it does (in order):

1. `helm uninstall` the release
2. Best-effort delete of the cluster-scoped `IngressClass` and `ClusterRoleBinding` / `ClusterRole` (Helm scopes these correctly but leftover state from failed installs is common)
3. Leaves the namespace intact — drop manually with `kubectl delete namespace nvelox-ingress` once you've confirmed nothing else lives there

`make uninstall` is idempotent — safe to re-run after a partial teardown.

## Upgrading

```bash
make install IMG=ghcr.io/nvelox/nvelox-ingress-controller:v0.2.0
```

`helm upgrade --install` handles the rest. Rolling update strategy: a brief moment of overlap where both old and new pods serve, then the old one terminates. `shareProcessNamespace` is preserved across pod restarts.

To see what would change before applying:

```bash
make helm-diff IMG=…   # requires `helm plugin install helm-diff`
```

## End-to-end smoke test

Two modes, depending on what cluster you have handy.

### Against your current cluster

The default. Assumes whatever `KUBECONFIG` / `kubectl config current-context` points at. `IMG` must be a registry-qualified tag the cluster can pull.

```bash
# If the registry is private + dev box has push access:
docker login registry.example.com
make smoke IMG=registry.example.com/nvelox-ingress-controller:dev PUSH=1

# If IMG is already pushed (CI built it, etc.):
make smoke IMG=registry.example.com/nvelox-ingress-controller:dev

# If you want a specific kubeconfig:
KUBECONFIG=/path/to/kube make smoke IMG=…
```

The test discovers the NodePort the Service got assigned and the first Node's `InternalIP`, then curls through. Override with `TARGET_URL=http://hostname:port` if your network needs something specific (MetalLB, externalIP, kube-proxy LB, etc.).

### Throwaway kind cluster

For "I just want to verify my change end-to-end without touching real infra".

```bash
make smoke USE_KIND=1                 # creates kind cluster, side-loads image, runs test, tears down
make smoke USE_KIND=1 KEEP=1          # leaves cluster running for inspection afterwards
```

`USE_KIND=1` mode side-loads the image into kind's internal containerd via `kind load docker-image`, so **no registry push is needed** and `IMG` defaults to a local-only tag.

### What it actually does

1. Build (`docker build`) + ship (`kind load` or `docker push`) the controller image
2. `helm upgrade --install` the chart
3. Curl the controller's HTTP port — must return `404` (default-backend listener is bound, catch-all firing)
4. Apply `samples/01-basic-http.yaml`, wait for the `"nvelox reloaded"` log line from the controller
5. Curl with `Host: echo.example.com` — must return `hello-from-nvelox`

Pass/fail is the exit code. Suitable for both local "did my change break it?" and CI gates.

### Running smoke in CI

`.github/workflows/ci.yml` ships with a `smoke` job that runs `make smoke USE_KIND=1` on every push + PR. It's gated on a single repo variable:

| Setting             | Value                                          | Where to set                |
|---|---|---|
| `NVELOX_IMG`        | Reachable nvelox sidecar image, e.g. `ghcr.io/yourorg/nvelox:v0.1.0` | Repo Settings → Variables   |

Without `NVELOX_IMG`, the smoke job is **skipped cleanly** (the chart's default sidecar is a private-registry image GitHub-hosted runners can't pull, so the job would otherwise always fail on forks / fresh clones). The `lint`, `test`, and `helm-lint` jobs always run regardless — those are the fast-feedback gate.

For an ad-hoc smoke run against a different sidecar build:

```
gh workflow run ci.yml -f nvelox_image=registry.example.com/nvelox/nvelox:dev
```

Private registry? Uncomment the registry-login step in `.github/workflows/ci.yml` and set `REGISTRY_USER` / `REGISTRY_PASS` / `REGISTRY_HOST` repo secrets.

## HA

```bash
helm upgrade nvelox-ingress … --set replicas=2 --set leaderElection.enabled=true
```

Leader election uses the Coordination API in the install namespace — make sure your RBAC bundle includes the `leader-election` Role (the chart adds it automatically when `leaderElection.enabled=true`).
