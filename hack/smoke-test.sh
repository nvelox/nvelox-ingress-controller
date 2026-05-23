#!/usr/bin/env bash
# smoke-test.sh — end-to-end verification.
#
# What it does (regardless of cluster mode):
#   1. Pick a cluster + image plumbing strategy (see modes below)
#   2. Helm install the chart, wait for rollout
#   3. Curl the default-backend (expect 404 — listener is bound)
#   4. Apply samples/01-basic-http.yaml, wait for the controller to render
#   5. Curl the real route (expect "hello-from-nvelox")
#
# Pass / fail is the script's exit code. Designed for both local dev
# ("does my change still work end-to-end?") and CI ("did the PR break
# the install path?").
#
# ─── Cluster mode ──────────────────────────────────────────────────
# default: use the current kubectl context (honors KUBECONFIG env).
#          The cluster MUST be able to pull $IMG — push first if it
#          lives on a remote registry. No teardown.
#
# USE_KIND=1: create (or reuse) a throwaway kind cluster. Side-loads
#             $IMG via `kind load docker-image` so no registry push
#             is needed. Tears down on exit unless KEEP=1.
#
# ─── Image plumbing ────────────────────────────────────────────────
# IMG          controller image                (default depends on mode — see below)
# NVELOX_IMG   nvelox sidecar image            (default: ghcr.io/nvelox/nvelox:latest)
# SKIP_BUILD=1 don't run `docker build` (assumes IMG already exists)
# PUSH=1       `docker push $IMG` after building. Required for non-kind
#              clusters when your dev box != the cluster nodes. The
#              registry credentials must already be in your local
#              docker config (`docker login` ahead of time).
#
# ─── Cluster knobs ─────────────────────────────────────────────────
# KUBECONFIG   honored by kubectl + helm natively (current context)
# CLUSTER      kind cluster name              (only used when USE_KIND=1; default nvelox-ingress-e2e)
# NODE_PORT    NodePort on the host           (default 30080 — kind binds it explicitly;
#              real clusters use whatever NodePort the Service got — see below)
# NAMESPACE    install namespace              (default nvelox-ingress)
# KEEP=1       skip teardown of the kind cluster

set -euo pipefail

USE_KIND="${USE_KIND:-0}"
CLUSTER="${CLUSTER:-nvelox-ingress-e2e}"
# NVELOX_IMG is intentionally unset by default — when empty, we DON'T
# pass `--set nvelox.image.*` and the chart's own default
# (values.yaml: nvelox.image.repository/tag) wins. Override only when
# you want to pin a non-default sidecar build for this run.
NVELOX_IMG="${NVELOX_IMG:-}"
NODE_PORT="${NODE_PORT:-30080}"
NAMESPACE="${NAMESPACE:-nvelox-ingress}"
KEEP="${KEEP:-0}"
SKIP_BUILD="${SKIP_BUILD:-0}"
PUSH="${PUSH:-0}"

# Image default depends on mode. kind happily uses a local-only tag;
# real clusters need a registry-qualified one — fail loudly if the
# operator forgot to set it.
if [[ -z "${IMG:-}" ]]; then
    if [[ "$USE_KIND" == "1" ]]; then
        IMG="ghcr.io/nvelox/nvelox-ingress-controller:e2e"
    else
        echo "✗ IMG required (e.g. IMG=registry.example.com/nvelox-ingress-controller:dev)" >&2
        echo "  For kind clusters: USE_KIND=1 ./hack/smoke-test.sh" >&2
        exit 1
    fi
fi

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"

# ─── Pre-flight ────────────────────────────────────────────────────

required_cmds=(kubectl helm docker)
if [[ "$USE_KIND" == "1" ]]; then required_cmds+=(kind); fi
for cmd in "${required_cmds[@]}"; do
    command -v "$cmd" >/dev/null || { echo "✗ missing required command: $cmd"; exit 1; }
done

log() { echo "==> $*" >&2; }
fail() { echo "✗ $*" >&2; exit 1; }

cleanup() {
    if [[ "$USE_KIND" != "1" ]]; then return; fi   # only kind clusters are throwaway
    if [[ "$KEEP" == "1" ]]; then
        log "KEEP=1; leaving kind cluster $CLUSTER running"
        return
    fi
    log "tearing down kind cluster $CLUSTER"
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# ─── Step 1a: cluster (kind mode) ──────────────────────────────────

if [[ "$USE_KIND" == "1" ]]; then
    if kind get clusters | grep -qx "$CLUSTER"; then
        log "reusing existing kind cluster $CLUSTER"
    else
        log "creating kind cluster $CLUSTER (NodePort :$NODE_PORT exposed on host)"
        cat <<EOF | kind create cluster --name "$CLUSTER" --config -
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    extraPortMappings:
      - containerPort: $NODE_PORT
        hostPort: $NODE_PORT
        protocol: TCP
EOF
    fi
    kind export kubeconfig --name "$CLUSTER" >/dev/null
    kubectl config use-context "kind-$CLUSTER" >/dev/null
fi

# ─── Step 1b: cluster (default mode — use current context) ─────────

CURRENT_CTX="$(kubectl config current-context 2>/dev/null || true)"
[[ -z "$CURRENT_CTX" ]] && fail "no current kubectl context (set KUBECONFIG or kubectl config use-context …)"
log "using cluster: $CURRENT_CTX"

# ─── Step 2: build + ship image ────────────────────────────────────

if [[ "$SKIP_BUILD" != "1" ]]; then
    log "building $IMG"
    docker build -t "$IMG" . >/dev/null
else
    log "SKIP_BUILD=1; assuming $IMG exists already"
fi

if [[ "$USE_KIND" == "1" ]]; then
    log "side-loading $IMG into kind"
    kind load docker-image "$IMG" --name "$CLUSTER" >/dev/null
elif [[ "$PUSH" == "1" ]]; then
    log "pushing $IMG (cluster will pull from registry)"
    docker push "$IMG" >/dev/null
else
    log "PUSH not set — assuming the cluster can already pull $IMG"
    log "  (set PUSH=1 to docker-push first, or pre-pull on each node)"
fi

# ─── Step 3: helm install ──────────────────────────────────────────

log "helm install (namespace: $NAMESPACE)"
HELM_ARGS=(
    upgrade --install nvelox-ingress-controller
    "$REPO_ROOT/deploy/helm/nvelox-ingress-controller"
    --namespace "$NAMESPACE" --create-namespace
    --set "controller.image.repository=$(echo "$IMG" | sed 's/:[^:]*$//')"
    --set "controller.image.tag=$(echo "$IMG" | sed 's/.*://')"
    # pullPolicy=Always so a new push to the same tag actually
    # gets re-pulled by kubelet. Without this, IfNotPresent +
    # mutable tag = "you pushed but the cluster still runs the
    # old bits". We saw this exact failure mode with :e2e tags.
    --set "controller.image.pullPolicy=Always"
    --set "service.type=NodePort"
)
# Only pin the nvelox sidecar image when explicitly overridden. With
# NVELOX_IMG unset, the chart's own values.yaml default wins — which
# is what most operators want once they've pinned their preferred
# repository there.
if [[ -n "$NVELOX_IMG" ]]; then
    HELM_ARGS+=(
        --set "nvelox.image.repository=$(echo "$NVELOX_IMG" | sed 's/:[^:]*$//')"
        --set "nvelox.image.tag=$(echo "$NVELOX_IMG" | sed 's/.*://')"
    )
fi
# Only pin the NodePort in kind mode where we control the host
# binding. On real clusters, let Kubernetes pick to avoid colliding
# with whatever else lives in the cluster's NodePort range.
if [[ "$USE_KIND" == "1" ]]; then
    HELM_ARGS+=(--set "service.httpNodePort=$NODE_PORT")
fi
helm "${HELM_ARGS[@]}" >/dev/null

# Force a rollout even when helm sees no spec change. Common path:
# we re-push the same `:dev` / `:e2e` tag with new content; helm
# treats the values as unchanged → no rollout → old pod keeps
# serving the old code. `rollout restart` annotates the pod template
# with a fresh timestamp so Kubernetes spins up new pods (which,
# with pullPolicy=Always above, actually pull the fresh digest).
log "forcing rollout to pick up any re-pushed image"
kubectl -n "$NAMESPACE" rollout restart deploy/nvelox-ingress-controller >/dev/null

log "waiting for controller rollout"
kubectl -n "$NAMESPACE" rollout status deploy/nvelox-ingress-controller --timeout=120s

# ─── Step 4: pick a reachable endpoint ─────────────────────────────

# kind mode: hit 127.0.0.1:$NODE_PORT directly (extraPortMappings).
# Other clusters: discover the NodePort the Service actually got and
# pick a Node IP. Works on most setups; the operator can override
# via TARGET_URL=http://x.y.z.w:port if their network needs something
# specific (kube-proxy LB, MetalLB, externalIP, etc).

if [[ "$USE_KIND" == "1" ]]; then
    TARGET_URL="${TARGET_URL:-http://127.0.0.1:$NODE_PORT}"
else
    if [[ -z "${TARGET_URL:-}" ]]; then
        NODE_PORT_LIVE="$(kubectl -n "$NAMESPACE" get svc nvelox-ingress-controller \
            -o jsonpath='{.spec.ports[?(@.name=="http")].nodePort}')"
        NODE_IP="$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')"
        [[ -z "$NODE_PORT_LIVE" || -z "$NODE_IP" ]] && \
            fail "couldn't auto-discover NodePort endpoint; set TARGET_URL=http://node:port and re-run"
        TARGET_URL="http://$NODE_IP:$NODE_PORT_LIVE"
    fi
fi
log "target endpoint: $TARGET_URL"

# ─── Step 5: default-backend should serve 404 ──────────────────────

log "probing default-backend (expect HTTP 404)"
for i in 1 2 3 4 5; do
    code="$(curl -s -o /dev/null -w '%{http_code}' "$TARGET_URL/" || echo 000)"
    if [[ "$code" == "404" ]]; then
        log "  → got 404 ✓ (port bound, catch-all firing)"
        break
    fi
    if [[ $i == 5 ]]; then
        kubectl -n "$NAMESPACE" logs -l app.kubernetes.io/name=nvelox-ingress-controller --all-containers --tail=80 || true
        fail "expected 404 from $TARGET_URL/ after 5 retries, got $code"
    fi
    sleep 2
done

# ─── Step 6: apply sample Ingress + wait for reload ────────────────

log "applying samples/01-basic-http.yaml"
kubectl apply -f "$REPO_ROOT/samples/01-basic-http.yaml" >/dev/null
kubectl rollout status deploy/echo --timeout=60s >/dev/null

log "waiting for nvelox reload"
for i in $(seq 1 15); do
    if kubectl -n "$NAMESPACE" logs -l app.kubernetes.io/name=nvelox-ingress-controller \
         -c controller --tail=50 2>/dev/null | grep -q "nvelox reloaded"; then
        break
    fi
    sleep 1
done

# ─── Step 7: real route returns the expected body ──────────────────

log "probing echo route (expect 'hello-from-nvelox')"
ok=0
for i in 1 2 3 4 5; do
    body="$(curl -s -H 'Host: echo.example.com' "$TARGET_URL/" || true)"
    if [[ "$body" == *hello-from-nvelox* ]]; then
        log "  → got 'hello-from-nvelox' ✓"
        ok=1
        break
    fi
    sleep 2
done
if [[ "$ok" != "1" ]]; then
    kubectl -n "$NAMESPACE" logs -l app.kubernetes.io/name=nvelox-ingress-controller --all-containers --tail=80 || true
    fail "expected response body containing 'hello-from-nvelox', got: $body"
fi

# Earlier version of the script gated each step on "wait for new
# 'nvelox reloaded' log line". That's brittle: when a previous run
# leaves the same objects in the cluster, `kubectl apply` is a no-op,
# reconcile produces an identical config, the reloader's hash-gate
# skips the write, no SIGHUP fires, no new log line — test times out
# even though the route is fine. The route-working curl is the real
# signal we care about; gating on that is both simpler and immune to
# leftover state from prior runs.

# ─── Step 8: named service-port resolution (#201) ──────────────────

log "VERIFYING named service ports (#201)"
log "  patching echo Service to give its port a name (idempotent)"
# `merge` patch with the same value is a no-op when port.name is
# already set from a prior run — quieter than `--type=json` `add`
# which can complain about already-present paths.
kubectl patch svc echo --type=merge \
    -p '{"spec":{"ports":[{"port":5678,"targetPort":5678,"name":"http"}]}}' >/dev/null

log "  applying Ingress that uses port.name=http"
cat <<'YAML' | kubectl apply -f - >/dev/null
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: echo-named-port
spec:
  ingressClassName: nvelox
  rules:
    - host: echo-named.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: echo
                port:
                  name: http
YAML

log "  probing named-port route (expect 'hello-from-nvelox')"
ok=0
for i in 1 2 3 4 5 6 7 8 9 10; do
    body="$(curl -s -H 'Host: echo-named.example.com' "$TARGET_URL/" || true)"
    if [[ "$body" == *hello-from-nvelox* ]]; then
        log "  → got 'hello-from-nvelox' ✓ (named port resolved)"
        ok=1
        break
    fi
    sleep 2
done
if [[ "$ok" != "1" ]]; then
    kubectl -n "$NAMESPACE" exec deploy/nvelox-ingress-controller -c controller \
        -- cat /etc/nvelox/conf.d/k8s.yaml 2>/dev/null || true
    kubectl -n "$NAMESPACE" logs -l app.kubernetes.io/name=nvelox-ingress-controller --all-containers --tail=80 || true
    fail "named-port Ingress didn't route; body was: $body"
fi

# ─── Step 9: redirect-https annotation (#202) ──────────────────────

log "VERIFYING nvelox.io/redirect-https annotation (#202)"
command -v openssl >/dev/null || fail "openssl required for the TLS test step"

log "  generating self-signed cert for secure-test.example.com"
# Don't `trap … EXIT` here — would clobber the kind-cleanup trap set
# earlier. Manual rm at the end of the step is enough; mktemp under
# $TMPDIR self-cleans on reboot anyway.
TMPDIR_TLS="$(mktemp -d)"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
    -keyout "$TMPDIR_TLS/tls.key" -out "$TMPDIR_TLS/tls.crt" \
    -subj '/CN=secure-test.example.com' >/dev/null 2>&1

log "  creating kubernetes.io/tls Secret"
kubectl create secret tls secure-test-tls \
    --cert="$TMPDIR_TLS/tls.crt" --key="$TMPDIR_TLS/tls.key" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
rm -rf "$TMPDIR_TLS"

log "  applying Ingress with nvelox.io/redirect-https=true"
cat <<'YAML' | kubectl apply -f - >/dev/null
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: echo-redirect-https
  annotations:
    nvelox.io/redirect-https: "true"
spec:
  ingressClassName: nvelox
  tls:
    - hosts: [secure-test.example.com]
      secretName: secure-test-tls
  rules:
    - host: secure-test.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: echo
                port:
                  number: 5678
YAML

log "  probing HTTP path (expect 301 with Location: https://...)"
ok=0
for i in 1 2 3 4 5 6 7 8 9 10; do
    # -I = HEAD so we don't have to follow / consume the body.
    headers="$(curl -sI -H 'Host: secure-test.example.com' "$TARGET_URL/" || true)"
    code="$(printf '%s' "$headers" | awk 'NR==1{print $2}')"
    location="$(printf '%s' "$headers" | awk -F': ' '/^[Ll]ocation:/{print $2}' | tr -d '\r\n')"
    if [[ "$code" == "301" ]] && [[ "$location" == https://secure-test.example.com/* ]]; then
        log "  → got 301 → $location ✓"
        ok=1
        break
    fi
    sleep 2
done
if [[ "$ok" != "1" ]]; then
    kubectl -n "$NAMESPACE" exec deploy/nvelox-ingress-controller -c controller \
        -- cat /etc/nvelox/conf.d/k8s.yaml 2>/dev/null || true
    kubectl -n "$NAMESPACE" logs -l app.kubernetes.io/name=nvelox-ingress-controller --all-containers --tail=80 || true
    fail "expected 301 + Location: https://secure-test.example.com/, got code=$code location=$location"
fi

# ─── Step 10: rate-limit annotations (#203) ───────────────────────

log "VERIFYING nvelox.io/rate-limit-per-{second,minute} annotations (#203)"
log "  applying Ingress with rate-limit-per-second=5"
cat <<'YAML' | kubectl apply -f - >/dev/null
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: echo-rate-limited
  annotations:
    nvelox.io/rate-limit-per-second: "5"
spec:
  ingressClassName: nvelox
  rules:
    - host: throttled.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: echo
                port:
                  number: 5678
YAML

# Give the controller a beat to reconcile + reload.
sleep 3

log "  hammering with 60 quick requests (expect a mix of 200 + 429)"
codes_file="$(mktemp)"
for i in $(seq 1 60); do
    curl -s -o /dev/null -w '%{http_code}\n' \
        -H 'Host: throttled.example.com' \
        "$TARGET_URL/" >> "$codes_file" || true
done
got_200="$(grep -c '^200$' "$codes_file" || true)"
got_429="$(grep -c '^429$' "$codes_file" || true)"
rm -f "$codes_file"

log "  → 200 responses: $got_200  /  429 responses: $got_429"
# The exact split depends on burst + timing, but a 5/s limit hit with
# 60 quick requests should yield AT LEAST a handful of 429s. If we
# see zero 429s the rate-limit annotation didn't take effect.
if [[ "$got_429" -lt 1 ]]; then
    kubectl -n "$NAMESPACE" exec deploy/nvelox-ingress-controller -c controller \
        -- cat /etc/nvelox/conf.d/k8s.yaml 2>/dev/null || true
    fail "expected at least 1 HTTP 429 from 60 quick requests against rps=5; got $got_429"
fi
log "  → rate limit firing ✓"

# ─── Step 11: sticky-cookie annotation (#204) ─────────────────────

log "VERIFYING nvelox.io/sticky-cookie annotation (#204)"
log "  applying Ingress with sticky-cookie=NVELOX_SRV"
cat <<'YAML' | kubectl apply -f - >/dev/null
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: echo-sticky
  annotations:
    nvelox.io/sticky-cookie: "NVELOX_SRV"
spec:
  ingressClassName: nvelox
  rules:
    - host: sticky.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: echo
                port:
                  number: 5678
YAML

# Give the controller a beat to reconcile + reload.
sleep 3

log "  verifying rendered config contains sticky_session block"
# Pre-#210 (single backend.server per Service via kube-proxy), nvelox
# has nothing to stick TO — there's only one "server" and no LB
# decision to remember, so it doesn't emit Set-Cookie at runtime even
# though we wrote the sticky_session config. What we CAN verify is
# that (a) the config rendered with the right shape and (b) nvelox
# accepted the reload (otherwise the route would 404, not 200). Full
# runtime cookie behavior is gated on #210 (EndpointSlices →
# multi-server backends) and tested there when it lands.
rendered="$(kubectl -n "$NAMESPACE" exec deploy/nvelox-ingress-controller -c controller \
    -- cat /etc/nvelox/conf.d/k8s.yaml 2>/dev/null || true)"
for expect in "sticky_session:" "type: cookie" "cookie_name: NVELOX_SRV" "ttl: 1h"; do
    if ! printf '%s' "$rendered" | grep -qF -- "$expect"; then
        printf '%s\n' "$rendered"
        fail "expected '$expect' in rendered config but didn't find it"
    fi
done
log "  → sticky_session block rendered correctly ✓"

log "  verifying route still serves (proves nvelox accepted the reload)"
body="$(curl -s -H 'Host: sticky.example.com' "$TARGET_URL/" || true)"
if [[ "$body" != *session-hello* && "$body" != *throttled-hello* && "$body" != *hello-from-nvelox* ]]; then
    # Match any of the body strings any previous sample might have
    # left on the echo Deployment, since smoke step ordering can
    # leave different echoes serving across re-runs. As long as the
    # route returns SOMETHING from the echo backend, nvelox is OK.
    fail "sticky-host route didn't serve; body was: $body"
fi
log "  → sticky route serving ✓"

# ─── Step 12: CIDR ACL annotations (#205) ─────────────────────────

log "VERIFYING nvelox.io/allow-cidrs and deny-cidrs annotations (#205)"
log "  applying Ingress with allow-cidrs + deny-cidrs"
cat <<'YAML' | kubectl apply -f - >/dev/null
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: echo-acl
  annotations:
    nvelox.io/allow-cidrs: "10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,127.0.0.1/32"
    nvelox.io/deny-cidrs:  "10.99.0.0/16"
spec:
  ingressClassName: nvelox
  rules:
    - host: acl.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: echo
                port:
                  number: 5678
YAML

# Give the controller a beat to reconcile + reload.
sleep 3

log "  verifying rendered config carries both CIDR lists"
# Runtime ACL enforcement depends on nvelox seeing the right source IP
# (X-Forwarded-For trust, etc.), which is environment-specific. Verify
# what we control: the config rendered correctly and nvelox accepted
# the reload. End-to-end IP-based enforcement is left to operator
# verification per their L4 setup.
rendered="$(kubectl -n "$NAMESPACE" exec deploy/nvelox-ingress-controller -c controller \
    -- cat /etc/nvelox/conf.d/k8s.yaml 2>/dev/null || true)"
for expect in "ip_allowlist:" "- 10.0.0.0/8" "- 172.16.0.0/12" "- 192.168.0.0/16" "ip_denylist:" "- 10.99.0.0/16"; do
    if ! printf '%s' "$rendered" | grep -qF -- "$expect"; then
        printf '%s\n' "$rendered"
        fail "expected '$expect' in rendered config but didn't find it"
    fi
done
log "  → both CIDR lists rendered correctly ✓"

# Bad CIDR in the value should NOT break the apply.
log "  applying Ingress with one bad CIDR mixed in (must not block reconcile)"
cat <<'YAML' | kubectl apply -f - >/dev/null
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: echo-acl-tolerant
  annotations:
    nvelox.io/deny-cidrs: "1.2.3.0/24,not-a-cidr,5.6.7.0/24"
spec:
  ingressClassName: nvelox
  rules:
    - host: tolerant.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: echo
                port:
                  number: 5678
YAML
sleep 3

body="$(curl -s -H 'Host: tolerant.example.com' "$TARGET_URL/" || true)"
if [[ -z "$body" || "$body" == *"404"* || "$body" == *not-found* ]]; then
    fail "bad-CIDR Ingress should still route the rest of the rules; got empty/404: $body"
fi
log "  → bad CIDR dropped, other rules still routing ✓"

# ─── Step 13: strip-prefix annotation (#206) ──────────────────────

log "VERIFYING nvelox.io/strip-prefix annotation (#206)"
log "  applying Ingress with strip-prefix=/api"
cat <<'YAML' | kubectl apply -f - >/dev/null
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: echo-stripped
  annotations:
    nvelox.io/strip-prefix: "/api"
spec:
  ingressClassName: nvelox
  rules:
    - host: stripped.example.com
      http:
        paths:
          - path: /api/v1
            pathType: Prefix
            backend:
              service:
                name: echo
                port:
                  number: 5678
YAML
sleep 3

log "  verifying rendered route uses path_regex + rewrite"
rendered="$(kubectl -n "$NAMESPACE" exec deploy/nvelox-ingress-controller -c controller \
    -- cat /etc/nvelox/conf.d/k8s.yaml 2>/dev/null || true)"
for expect in 'path_regex: ^/api/v1(.*)$' 'path: /v1$1'; do
    if ! printf '%s' "$rendered" | grep -qF -- "$expect"; then
        printf '%s\n' "$rendered"
        fail "expected '$expect' in rendered config but didn't find it"
    fi
done
log "  → strip-prefix rewrite rendered correctly ✓"

# ─── Step 14: header injection (#207) ─────────────────────────────

log "VERIFYING nvelox.io/{request,response}-headers annotations (#207)"
cat <<'YAML' | kubectl apply -f - >/dev/null
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: echo-headered
  annotations:
    nvelox.io/request-headers: |
      X-Forwarded-By: nvelox-smoke
    nvelox.io/response-headers: |
      X-Smoke-Test: passed
spec:
  ingressClassName: nvelox
  rules:
    - host: headered.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: echo
                port:
                  number: 5678
YAML
sleep 3

log "  probing for X-Smoke-Test response header"
ok=0
for i in 1 2 3 4 5 6 7 8 9 10; do
    headers="$(curl -sI -H 'Host: headered.example.com' "$TARGET_URL/" || true)"
    if printf '%s' "$headers" | grep -iE '^X-Smoke-Test:[[:space:]]*passed' >/dev/null; then
        log "  → response carries X-Smoke-Test: passed ✓"
        ok=1
        break
    fi
    sleep 2
done
if [[ "$ok" != "1" ]]; then
    kubectl -n "$NAMESPACE" exec deploy/nvelox-ingress-controller -c controller \
        -- cat /etc/nvelox/conf.d/k8s.yaml 2>/dev/null || true
    fail "expected X-Smoke-Test: passed response header, got:\n$headers"
fi

# ─── Step 15: Ingress status update (#208) ────────────────────────

log "VERIFYING Ingress.status.loadBalancer is published (#208)"
ok=0
for i in 1 2 3 4 5 6 7 8 9 10; do
    # Inspect echo Ingress from sample 01 — should now have IPs.
    addrs="$(kubectl get ingress echo -o jsonpath='{.status.loadBalancer.ingress[*].ip}' 2>/dev/null || true)"
    if [[ -n "$addrs" ]]; then
        log "  → echo Ingress status.loadBalancer.ingress[].ip = $addrs ✓"
        ok=1
        break
    fi
    sleep 2
done
if [[ "$ok" != "1" ]]; then
    fail "expected echo Ingress to have status.loadBalancer.ingress[].ip set within 20s"
fi

# ─── Step 16: EndpointSlices-based backend routing (#210) ─────────

log "VERIFYING per-pod EndpointSlices routing (#210)"
log "  scaling echo Deployment to 3 replicas"
kubectl scale deploy echo --replicas=3 >/dev/null
kubectl rollout status deploy/echo --timeout=60s >/dev/null

# Wait for EndpointSlices to settle.
sleep 5

log "  verifying rendered backend has 3 pod-IP servers (not Service DNS)"
# Every smoke Ingress points at the echo Service:5678, so they all
# collapse to a single backend (k8s-default-echo-5678) in the
# rendered config. That backend's `servers:` list is the only place
# where `  - <addr>:5678` lines appear at top-of-list indentation —
# count them across the WHOLE document instead of trying to extract
# the block (an earlier awk attempt mis-detected the block boundary
# because YAML list items start with `-`, matching the same "next
# non-space line" terminator we used).
ok=0
for i in 1 2 3 4 5 6 7 8 9 10; do
    rendered="$(kubectl -n "$NAMESPACE" exec deploy/nvelox-ingress-controller -c controller \
        -- cat /etc/nvelox/conf.d/k8s.yaml 2>/dev/null || true)"
    pod_count="$(printf '%s' "$rendered" | grep -cE '^  - [0-9]+\.[0-9]+\.[0-9]+\.[0-9]+:5678$' || true)"
    has_dns="$(printf '%s' "$rendered" | grep -c 'echo\.default\.svc\.cluster\.local' || true)"
    if [[ "$pod_count" == "3" && "$has_dns" == "0" ]]; then
        log "  → 3 pod-IP servers in backend, Service DNS replaced ✓"
        ok=1
        break
    fi
    sleep 2
done
if [[ "$ok" != "1" ]]; then
    printf '%s\n' "$rendered"
    fail "expected 3 pod-IP servers + 0 Service-DNS entries; got pod=$pod_count dns=$has_dns"
fi

# Reset replica count so subsequent runs don't drift.
kubectl scale deploy echo --replicas=1 >/dev/null

# ─── Step 17: IngressClass parameters (#213) ──────────────────────

log "VERIFYING IngressClass.spec.parameters → ConfigMap defaults (#213)"
log "  creating defaults ConfigMap"
cat <<'YAML' | kubectl apply -f - >/dev/null
apiVersion: v1
kind: ConfigMap
metadata:
  name: nvelox-class-defaults-smoke
  namespace: nvelox-ingress
data:
  rate-limit-per-second: "50"
  response-headers: |
    X-Class-Default: smoke
YAML

log "  patching IngressClass to reference the defaults"
# Omit apiGroup entirely for core resources (ConfigMap) — the field
# is *string; the API server's RFC-1123 validator rejects "" but
# accepts nil (which means "core group").
kubectl patch ingressclass nvelox --type=merge \
    -p '{"spec":{"parameters":{"kind":"ConfigMap","name":"nvelox-class-defaults-smoke","namespace":"nvelox-ingress","scope":"Namespace"}}}' >/dev/null

log "  applying Ingress with NO annotations (should inherit defaults)"
cat <<'YAML' | kubectl apply -f - >/dev/null
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: echo-inherits
spec:
  ingressClassName: nvelox
  rules:
    - host: inherits.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: echo
                port:
                  number: 5678
YAML
sleep 3

log "  verifying rendered config carries the class-default ip_rate_limit + response header"
rendered="$(kubectl -n "$NAMESPACE" exec deploy/nvelox-ingress-controller -c controller \
    -- cat /etc/nvelox/conf.d/k8s.yaml 2>/dev/null || true)"
for expect in 'requests_per_second: 50' 'X-Class-Default: smoke'; do
    if ! printf '%s' "$rendered" | grep -qF -- "$expect"; then
        printf '%s\n' "$rendered"
        fail "expected class-default '$expect' to apply to inheriting Ingress; not found"
    fi
done
log "  → class defaults flowed through to the unannotated Ingress ✓"

# Clear the parameters reference so the next smoke run starts clean.
kubectl patch ingressclass nvelox --type=json \
    -p '[{"op":"remove","path":"/spec/parameters"}]' >/dev/null 2>&1 || true

log ""
log "smoke test PASSED — basic + #201 + #202 + #203 + #204 + #205 + #206 + #207 + #208 + #210 + #213"
