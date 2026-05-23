# nvelox-ingress-controller — install/uninstall + dev workflow.
#
# Modeled on the ngris-operator Makefile so the same `make install
# KUBECONFIG=… IMG=…` muscle memory works here.
#
# Pass KUBECONFIG=/path/to/file to target a specific cluster.
# Falls back to the KUBECONFIG env var, then to kubectl's default
# (~/.kube/config). Both kubectl and helm honor this var natively.
KUBECONFIG ?=
KUBECTL    := kubectl $(if $(KUBECONFIG),--kubeconfig=$(KUBECONFIG))
HELM       := helm    $(if $(KUBECONFIG),--kubeconfig=$(KUBECONFIG))

# Names — override if you run more than one instance per cluster, or
# if your platform team standardizes on a different release name.
# RELEASE intentionally matches the chart name so Helm's fullname
# helper doesn't double up to `<release>-<chart>` on every resource.
RELEASE   ?= nvelox-ingress-controller
NAMESPACE ?= nvelox-ingress
CHART_DIR := deploy/helm/nvelox-ingress-controller

# ─── Dev loop ──────────────────────────────────────────────────────

.PHONY: build
build:
	go build -o bin/nvelox-ingress-controller .

.PHONY: test
test:
	go test ./... -timeout 60s

# End-to-end smoke test. Builds the image, helm-installs the chart,
# applies the basic sample, asserts that the port serves the default-
# backend 404 AND the real route.
#
# Modes:
#   make smoke USE_KIND=1                       — throwaway kind cluster, side-loads IMG
#   make smoke IMG=registry.example.com/x:tag PUSH=1   — your current kubectl context,
#                                                pushes IMG to registry first
#   make smoke IMG=…                            — your current kubectl context,
#                                                assumes IMG already pullable
#
# See hack/smoke-test.sh top-of-file comment for every knob.
.PHONY: smoke
smoke:
	./hack/smoke-test.sh

.PHONY: vet
vet:
	go vet ./...

.PHONY: tidy
tidy:
	go mod tidy

.PHONY: fmt
fmt:
	gofmt -s -w .

# Local container build — image name matches the chart's default
# repository so `make install IMG=ghcr.io/nvelox/nvelox-ingress-controller:dev`
# works after a `docker push`.
.PHONY: docker-build
docker-build:
	@: $${IMG:?required (e.g. IMG=ghcr.io/nvelox/nvelox-ingress-controller:dev)}
	docker build -t $(IMG) .

# Kind helper — load a locally-built image into a kind cluster so
# `imagePullPolicy: IfNotPresent` finds it without a registry push.
.PHONY: kind-load
kind-load:
	@: $${IMG:?required}
	kind load docker-image $(IMG) $(if $(KIND_CLUSTER),--name=$(KIND_CLUSTER),)

# ─── Install / Uninstall ───────────────────────────────────────────

# Helm chart install. Idempotent: re-running upgrades in place.
#
# Required:
#   IMG — controller image (e.g. ghcr.io/nvelox/nvelox-ingress-controller:v0.1.0)
#
# Optional:
#   INGRESS_CLASS  — override the IngressClass name (default: nvelox)
#   DEFAULT_CLASS  — set to true to mark the class as cluster-default
#   SERVICE_TYPE   — LoadBalancer (default), NodePort, ClusterIP
#   NVELOX_IMG     — override the nvelox sidecar image
#   VALUES_FILE    — extra Helm values file to overlay
.PHONY: install
install:
	@: $${IMG:?required (e.g. IMG=ghcr.io/nvelox/nvelox-ingress-controller:v0.1.0)}
	@echo "==> 1/2 Namespace $(NAMESPACE)"
	@$(KUBECTL) create namespace $(NAMESPACE) 2>/dev/null || true
	@$(KUBECTL) annotate namespace $(NAMESPACE) \
	    meta.helm.sh/release-name=$(RELEASE) \
	    meta.helm.sh/release-namespace=$(NAMESPACE) --overwrite 2>/dev/null || true
	@$(KUBECTL) label namespace $(NAMESPACE) \
	    app.kubernetes.io/managed-by=Helm --overwrite 2>/dev/null || true
	@echo "==> 2/2 Helm install $(RELEASE)"
	@$(HELM) upgrade --install $(RELEASE) $(CHART_DIR) \
	    --namespace $(NAMESPACE) \
	    --set controller.image.repository="$(shell echo $(IMG) | sed 's/:[^:]*$$//')" \
	    --set controller.image.tag="$(shell echo $(IMG) | sed 's/.*://')" \
	    $(if $(INGRESS_CLASS),--set ingressClass.name=$(INGRESS_CLASS),) \
	    $(if $(DEFAULT_CLASS),--set ingressClass.default=$(DEFAULT_CLASS),) \
	    $(if $(SERVICE_TYPE),--set service.type=$(SERVICE_TYPE),) \
	    $(if $(NVELOX_IMG),--set nvelox.image.repository=$(shell echo $(NVELOX_IMG) | sed 's/:[^:]*$$//') --set nvelox.image.tag=$(shell echo $(NVELOX_IMG) | sed 's/.*://'),) \
	    $(if $(VALUES_FILE),--values $(VALUES_FILE),)
	@$(KUBECTL) -n $(NAMESPACE) rollout status deploy/$(RELEASE) --timeout=120s
	@echo "==> Installed. Try: make install-sample"

.PHONY: install-sample
install-sample:
	$(KUBECTL) apply -f samples/01-basic-http.yaml

# Clean uninstall — removes the Helm release, the cluster-scoped
# IngressClass/ClusterRole/ClusterRoleBinding, and the namespace
# (after best-effort emptying). Idempotent — safe to re-run.
.PHONY: uninstall
uninstall:
	@echo "==> 1/3 Helm uninstall $(RELEASE)"
	@$(HELM) uninstall $(RELEASE) -n $(NAMESPACE) 2>/dev/null || true
	@echo "==> 2/3 Removing cluster-scoped leftovers (ingressclass, RBAC)"
	@$(KUBECTL) delete ingressclass.networking.k8s.io \
	    -l app.kubernetes.io/instance=$(RELEASE) 2>/dev/null || true
	@$(KUBECTL) delete clusterrolebinding,clusterrole \
	    -l app.kubernetes.io/instance=$(RELEASE) 2>/dev/null || true
	@echo "==> 3/3 Done. Namespace $(NAMESPACE) left intact"
	@echo "    (run '$(KUBECTL) delete namespace $(NAMESPACE)' to remove)."

# ─── Helm helpers ──────────────────────────────────────────────────

.PHONY: helm-lint
helm-lint:
	helm lint $(CHART_DIR)

.PHONY: helm-template
helm-template:
	@helm template $(RELEASE) $(CHART_DIR) \
	    $(if $(IMG),--set controller.image.repository=$(shell echo $(IMG) | sed 's/:[^:]*$$//') --set controller.image.tag=$(shell echo $(IMG) | sed 's/.*://'),)

# CRD-style refresh, kept here for symmetry with the operator's Makefile
# even though this controller doesn't ship CRDs yet. When NveloxRoute
# (the Traefik IngressRoute analog) lands, point this at the CRDs dir.
.PHONY: helm-sync
helm-sync:
	@echo "no CRDs yet; placeholder for the eventual NveloxRoute CRD"

# Diff the rendered manifests against what's deployed — handy on
# upgrades to spot accidental selector/label drift.
.PHONY: helm-diff
helm-diff:
	@command -v helm-diff >/dev/null || { echo "install: helm plugin install https://github.com/databus23/helm-diff"; exit 1; }
	@$(HELM) diff upgrade $(RELEASE) $(CHART_DIR) --namespace $(NAMESPACE) \
	    $(if $(IMG),--set controller.image.repository=$(shell echo $(IMG) | sed 's/:[^:]*$$//') --set controller.image.tag=$(shell echo $(IMG) | sed 's/.*://'),)

# ─── Run controller out-of-cluster ─────────────────────────────────

# Targets the current kube-context. The controller will try to SIGHUP
# nvelox via /proc — outside Kubernetes that fails, so this mode is
# only useful for watching the translator output (set CONFIG_PATH to
# a temp file).
.PHONY: run
run: build
	./bin/nvelox-ingress-controller \
	    --ingress-class=$(or $(INGRESS_CLASS),nvelox) \
	    --config-path=/tmp/nvelox-k8s.yaml \
	    --pid-file=/tmp/nvelox.pid \
	    --tls-cert-dir=/tmp/nvelox-tls \
	    --metrics-bind-address=:8082 \
	    --health-probe-bind-address=:8083
