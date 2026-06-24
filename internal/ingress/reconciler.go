// Package ingress reconciles Ingress objects whose IngressClassName
// matches ours into an nvelox config fragment + on-disk TLS material.
//
// Reconcile fans-in: every Ingress / Secret change re-renders the WHOLE
// config (cheap; bounded by cluster ingress count). That avoids the
// classic "I just deleted a route but the file still has it" bug you
// hit if you try to do per-resource patching against a YAML file.
//
// Status updates back onto Ingress.status.loadBalancer are deliberately
// out of scope for v1 — they require knowing the Service's external
// IP, which is environment-specific (LoadBalancer vs NodePort vs
// hostPort). Add once we settle on a default Service shape.
package ingress

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/nvelox/nvelox-ingress-controller/internal/annotations"
	"github.com/nvelox/nvelox-ingress-controller/internal/reloader"
	"github.com/nvelox/nvelox-ingress-controller/internal/translator"
)

// Reconciler is wired by main against the controller-runtime manager.
// All long-lived state lives here so the per-call Reconcile method
// stays purely functional.
type Reconciler struct {
	client.Client
	IngressClass       string             // e.g. "nvelox"; matched against ingressClassName
	HTTPPort           int                // nvelox-side bind for HTTP listener
	HTTPSPort          int                // nvelox-side bind for HTTPS listener
	TLSCertDir         string             // shared volume mount inside nvelox container
	DefaultBackendRoot string             // empty dir mount used as nvelox `static.root` for the catch-all 404
	Reload             *reloader.Reloader // signals nvelox after a successful write

	// PublishService is the Service whose external address (LB
	// hostname/IP, or Node IPs for NodePort) gets written back to
	// every owned Ingress's status.loadBalancer.ingress[]. Form:
	// "<namespace>/<name>". Empty disables status updates.
	PublishService string

	// TrustedProxies is emitted as `trusted_proxies` on every generated
	// nvelox listener (see translator.Inputs.TrustedProxies). Set when
	// this nvelox runs behind an upstream proxy so it appends to, rather
	// than overwrites, the inbound X-Forwarded-For. Empty = no upstream
	// trusted (XFF replaced with peer — the safe default for a true edge).
	TrustedProxies []string
}

// Reconcile is fired on any Ingress / Secret event in the cluster. We
// don't act on the specific object — we just rebuild the whole world.
// The reloader's content-hash gate makes this cheap: identical desired
// state → no file write, no SIGHUP.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := slog.Default().With("trigger", req.NamespacedName.String())

	var all networkingv1.IngressList
	if err := r.List(ctx, &all); err != nil {
		return ctrl.Result{}, fmt.Errorf("list ingresses: %w", err)
	}

	ours := make([]networkingv1.Ingress, 0, len(all.Items))
	for _, ing := range all.Items {
		if !r.owns(&ing) {
			continue
		}
		ours = append(ours, ing)
	}

	// Materialize TLS secrets first so the rendered YAML never
	// references a file that isn't on disk yet. A failure here is
	// recoverable — we log and continue with whatever we managed
	// to write, and the reload picks up partial TLS until the next
	// Secret event re-fires reconcile.
	if r.TLSCertDir != "" {
		if err := r.syncTLSSecrets(ctx, ours); err != nil {
			logger.Warn("tls sync partial failure", "err", err)
		}
	}

	// Build the named-port resolution map from in-cluster Services.
	// Cheap List — kube-apiserver serves it from the controller-
	// runtime cache, no real round-trip in steady state.
	servicePorts, err := r.buildServicePortMap(ctx, ours)
	if err != nil {
		// Non-fatal — we just fall back to the v1 behavior of
		// dropping named-port routes. Real failure modes (api-
		// server down) will surface via the next reconcile too.
		logger.Warn("service-port lookup partial failure; named-port routes may drop", "err", err)
	}

	// Per-pod endpoint addresses via EndpointSlices. When this
	// returns empty for a (ns/svc/port), the translator falls back
	// to the Service DNS form — backward-compatible with pre-#210
	// deploys and with not-yet-Ready Services.
	endpointAddrs, err := r.buildEndpointAddressMap(ctx, ours, servicePorts)
	if err != nil {
		logger.Warn("endpoint lookup partial failure; some routes will use Service DNS fallback", "err", err)
	}

	// IngressClass parameters (#213) — load defaults once per
	// reconcile, then merge under each Ingress's own annotations.
	// Translator gets the pre-merged Spec via AnnotationOverrides
	// and skips re-parsing. When the class doesn't have parameters
	// (or the ref is malformed), defaults is the zero Spec and the
	// merge is a no-op — backward compatible.
	classDefaults := r.loadClassDefaults(ctx)
	overrides := map[string]annotations.Spec{}
	for _, ing := range ours {
		overrides[ing.Namespace+"/"+ing.Name] = annotations.Merge(classDefaults, annotations.Parse(&ing))
	}

	rendered, err := translator.Render(translator.Inputs{
		Ingresses:           ours,
		TLSCertDir:          r.TLSCertDir,
		HTTPPort:            r.HTTPPort,
		HTTPSPort:           r.HTTPSPort,
		DefaultBackendRoot:  r.DefaultBackendRoot,
		ServicePorts:        servicePorts,
		EndpointAddresses:   endpointAddrs,
		AnnotationOverrides: overrides,
		TrustedProxies:      r.TrustedProxies,
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("translate: %w", err)
	}

	changed, err := r.Reload.Apply(rendered)
	if err != nil {
		// Apply already wrote the file even when the SIGHUP failed
		// — bubble the error so controller-runtime re-queues with
		// backoff, but don't lose the new config.
		return ctrl.Result{}, fmt.Errorf("apply: %w", err)
	}
	if changed {
		logger.Info("nvelox config updated", "ingresses", len(ours))
	}

	// Status updates: write the fronting Service's external address
	// back onto every owned Ingress. Best-effort — a transient
	// failure here doesn't roll back the route render. Skip when
	// PublishService is empty (operator opted out).
	if r.PublishService != "" {
		if perr := r.publishStatus(ctx, ours); perr != nil {
			logger.Warn("ingress status update partial failure", "err", perr)
		}
	}
	return ctrl.Result{}, nil
}

// loadClassDefaults reads the IngressClass referenced by r.IngressClass,
// follows spec.parameters to a ConfigMap if one is configured, and
// returns the Spec parsed from that ConfigMap's data. Errors are
// non-fatal — they degrade to "no defaults" with a log so a typo'd
// or missing parameters reference doesn't block the reconcile.
//
// Supported parameters target: ConfigMap (apiGroup="", kind="ConfigMap").
// The full IngressClass.spec.parameters spec allows any resource ref;
// supporting custom CRDs as defaults is a follow-up — for now the
// ConfigMap form covers the cluster-wide knobs we'd encode in a
// typed CRD anyway, with no CRD plumbing overhead.
func (r *Reconciler) loadClassDefaults(ctx context.Context) annotations.Spec {
	var class networkingv1.IngressClass
	if err := r.Get(ctx, types.NamespacedName{Name: r.IngressClass}, &class); err != nil {
		// IngressClass might not exist yet in fresh clusters where
		// the chart hasn't installed it — caller (Reconcile) is
		// also fine with no class defaults, so this is silent.
		return annotations.Spec{}
	}
	p := class.Spec.Parameters
	if p == nil {
		return annotations.Spec{}
	}
	if p.Kind != "ConfigMap" || (p.APIGroup != nil && *p.APIGroup != "") {
		slog.Warn("ingressclass parameters: only ConfigMap targets supported today",
			"kind", p.Kind, "name", p.Name)
		return annotations.Spec{}
	}
	ns := "default"
	if p.Namespace != nil && *p.Namespace != "" {
		ns = *p.Namespace
	}
	var cm corev1.ConfigMap
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: p.Name}, &cm); err != nil {
		if !apierrors.IsNotFound(err) {
			slog.Warn("ingressclass parameters ConfigMap lookup failed",
				"namespace", ns, "name", p.Name, "err", err)
		}
		return annotations.Spec{}
	}
	// ConfigMap keys use the SAME shape as annotation keys (with the
	// nvelox.io/ prefix) so operators can copy-paste between an
	// Ingress annotation and a class defaults entry. Bare-key form
	// is also accepted as a convenience.
	normalized := normalizeClassParamKeys(cm.Data)
	return annotations.ParseAnnotations(
		"ingressclass:"+r.IngressClass+"/"+ns+"/"+p.Name,
		normalized,
	)
}

// normalizeClassParamKeys accepts ConfigMap data with either the full
// "nvelox.io/X" key form OR the bare "X" form, returning a map with
// every key prefixed. Bare form is easier to type in a ConfigMap
// (where the prefix is just noise — the ConfigMap isn't an Ingress);
// fully-prefixed form is also accepted so copy-paste from an Ingress
// annotations block works.
func normalizeClassParamKeys(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		if strings.HasPrefix(k, annotations.Prefix) {
			out[k] = v
			continue
		}
		out[annotations.Prefix+k] = v
	}
	return out
}

// buildEndpointAddressMap reads EndpointSlices for every backend
// referenced by `ours` and returns a "<ns>/<svc>/<port>" → ["ip:port",
// ...] map for the translator. Only Ready endpoints are included
// (matches kube-proxy's default filtering); not-Ready or terminating
// pods are excluded so we don't route to a draining backend.
//
// When the result is empty for a given key (no Ready endpoints yet,
// or EndpointSlices not synced), the translator falls back to the
// Service DNS form — that path keeps working through kube-proxy
// until the slices catch up.
func (r *Reconciler) buildEndpointAddressMap(ctx context.Context, ours []networkingv1.Ingress, servicePorts map[string]int32) (map[string][]string, error) {
	out := map[string][]string{}
	// Collect the (ns/svc/port) tuples we care about so we don't
	// scan EndpointSlices for unrelated Services.
	type want struct {
		ns, svc string
		port    int32
	}
	wanted := map[string]want{}
	for _, ing := range ours {
		for _, rule := range ing.Spec.Rules {
			if rule.HTTP == nil {
				continue
			}
			for _, p := range rule.HTTP.Paths {
				if p.Backend.Service == nil {
					continue
				}
				port := int32(0)
				if p.Backend.Service.Port.Number > 0 {
					port = p.Backend.Service.Port.Number
				} else if p.Backend.Service.Port.Name != "" {
					if n, ok := servicePorts[fmt.Sprintf("%s/%s/%s",
						ing.Namespace, p.Backend.Service.Name, p.Backend.Service.Port.Name)]; ok {
						port = n
					}
				}
				if port == 0 {
					continue
				}
				key := fmt.Sprintf("%s/%s/%d", ing.Namespace, p.Backend.Service.Name, port)
				wanted[key] = want{ns: ing.Namespace, svc: p.Backend.Service.Name, port: port}
			}
		}
	}

	// List EndpointSlices namespace-scoped per Service we care about.
	// EndpointSlices carry the standard "kubernetes.io/service-name"
	// label so a label-selector list is the cheap path.
	var firstErr error
	for _, w := range wanted {
		var sliceList discoveryv1.EndpointSliceList
		if err := r.List(ctx, &sliceList,
			client.InNamespace(w.ns),
			client.MatchingLabels{discoveryv1.LabelServiceName: w.svc},
		); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("list endpointslices %s/%s: %w", w.ns, w.svc, err)
			}
			continue
		}
		key := fmt.Sprintf("%s/%s/%d", w.ns, w.svc, w.port)
		seen := map[string]struct{}{}
		for _, sl := range sliceList.Items {
			// Find the port number on this slice that matches what
			// the Ingress wants. EndpointSlices can list multiple
			// ports per slice; match by .port.
			targetPort := int32(0)
			for _, sp := range sl.Ports {
				if sp.Port != nil && *sp.Port == w.port {
					targetPort = *sp.Port
					break
				}
			}
			if targetPort == 0 {
				continue
			}
			for _, ep := range sl.Endpoints {
				// Skip not-ready endpoints — pods in CrashLoop /
				// terminating / failing readinessProbe shouldn't
				// receive traffic. Matches kube-proxy default.
				if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
					continue
				}
				for _, addr := range ep.Addresses {
					if addr == "" {
						continue
					}
					a := fmt.Sprintf("%s:%d", addr, targetPort)
					if _, dup := seen[a]; dup {
						continue
					}
					seen[a] = struct{}{}
					out[key] = append(out[key], a)
				}
			}
		}
	}
	return out, firstErr
}

// publishStatus reads the controller's fronting Service, derives the
// public IngressLoadBalancerIngress list, and patches every owned
// Ingress's status.loadBalancer.ingress[] to match. Idempotent: when
// the current status already matches, no patch is sent.
func (r *Reconciler) publishStatus(ctx context.Context, ours []networkingv1.Ingress) error {
	desired, err := r.fetchPublishAddresses(ctx)
	if err != nil {
		return fmt.Errorf("publish addresses: %w", err)
	}
	if len(desired) == 0 {
		// Service exists but no addresses yet (LB still provisioning,
		// no Node IPs visible). Skip silently — the Service event
		// when the LB lands re-fires reconcile.
		return nil
	}
	var firstErr error
	for i := range ours {
		ing := &ours[i]
		if ingressStatusEqual(ing.Status.LoadBalancer.Ingress, desired) {
			continue
		}
		patch := client.MergeFrom(ing.DeepCopy())
		ing.Status.LoadBalancer.Ingress = desired
		if err := r.Status().Patch(ctx, ing, patch); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("patch %s/%s status: %w",
					ing.Namespace, ing.Name, err)
			}
		}
	}
	return firstErr
}

// fetchPublishAddresses reads the configured Service and returns the
// IngressLoadBalancerIngress entries to publish.
//   - LoadBalancer Service: read .status.loadBalancer.ingress (the
//     external IP / hostname assigned by the cloud LB / MetalLB).
//   - NodePort Service: list the cluster's Nodes and use their
//     InternalIPs (closest thing to a published address without an
//     external LB).
//   - ClusterIP Service: return nothing (no externally-reachable
//     address — typical for "fronted by something else" topologies;
//     skip rather than publish a misleading address).
func (r *Reconciler) fetchPublishAddresses(ctx context.Context) ([]networkingv1.IngressLoadBalancerIngress, error) {
	ns, name, ok := strings.Cut(r.PublishService, "/")
	if !ok || ns == "" || name == "" {
		return nil, fmt.Errorf("invalid PublishService %q (expected <ns>/<name>)", r.PublishService)
	}
	var svc corev1.Service
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &svc); err != nil {
		return nil, err
	}
	var out []networkingv1.IngressLoadBalancerIngress
	switch svc.Spec.Type {
	case corev1.ServiceTypeLoadBalancer:
		for _, e := range svc.Status.LoadBalancer.Ingress {
			out = append(out, networkingv1.IngressLoadBalancerIngress{
				IP: e.IP, Hostname: e.Hostname,
			})
		}
	case corev1.ServiceTypeNodePort:
		var nodes corev1.NodeList
		if err := r.List(ctx, &nodes); err != nil {
			return nil, fmt.Errorf("list nodes: %w", err)
		}
		for _, n := range nodes.Items {
			for _, addr := range n.Status.Addresses {
				if addr.Type == corev1.NodeInternalIP && addr.Address != "" {
					out = append(out, networkingv1.IngressLoadBalancerIngress{IP: addr.Address})
					break // one IP per node is enough
				}
			}
		}
	}
	return out, nil
}

// ingressStatusEqual is a shallow comparator — equal iff both lists
// have the same length and matching {IP, Hostname} entries in order.
// Status updates already arrive in the order we wrote them, so order
// is stable across reconciles.
func ingressStatusEqual(a, b []networkingv1.IngressLoadBalancerIngress) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].IP != b[i].IP || a[i].Hostname != b[i].Hostname {
			return false
		}
	}
	return true
}

// owns returns true when this Ingress belongs to us. Two ways to claim
// it: an explicit ingressClassName matching our class, OR an
// IngressClass resource elsewhere marked as default + matching our
// controller name (deferred to v2 — we only honor explicit class for
// the v1 cut to keep cluster-wide adoption opt-in).
func (r *Reconciler) owns(ing *networkingv1.Ingress) bool {
	if ing == nil {
		return false
	}
	if ing.Spec.IngressClassName == nil {
		return false
	}
	return *ing.Spec.IngressClassName == r.IngressClass
}

// syncTLSSecrets reads every Secret referenced by `ours` and writes
// `<ns>-<name>.crt` + `.key` under TLSCertDir. Secret format is the
// standard kubernetes.io/tls shape: data["tls.crt"], data["tls.key"].
//
// After writing the keep-set, prunes any *.crt / *.key under
// TLSCertDir that's NOT in the keep-set — leftover material from
// previously-referenced Secrets that nothing points at anymore.
// Order matters: prune AFTER the new files are written, so a TLS
// rotation that replaces a Secret with a new one (different name)
// never has a window where neither file exists.
func (r *Reconciler) syncTLSSecrets(ctx context.Context, ours []networkingv1.Ingress) error {
	if err := os.MkdirAll(r.TLSCertDir, 0o750); err != nil {
		return fmt.Errorf("mkdir %s: %w", r.TLSCertDir, err)
	}
	seen := map[string]struct{}{}
	// keep is the set of basenames we wrote / would have written
	// (even if Secret is missing right now — we don't want to
	// prune a stale file just because the Secret is briefly absent
	// mid-rotation). Format: "<ns>-<name>.crt" / "<ns>-<name>.key".
	keep := map[string]struct{}{}
	var firstErr error
	for _, ing := range ours {
		for _, t := range ing.Spec.TLS {
			if t.SecretName == "" {
				continue
			}
			key := ing.Namespace + "/" + t.SecretName
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			// Record the desired files BEFORE attempting the Get —
			// a missing Secret is a "wait for it" state, not a
			// "prune the existing files" state.
			keep[ing.Namespace+"-"+t.SecretName+".crt"] = struct{}{}
			keep[ing.Namespace+"-"+t.SecretName+".key"] = struct{}{}

			var sec corev1.Secret
			err := r.Get(ctx, types.NamespacedName{
				Namespace: ing.Namespace,
				Name:      t.SecretName,
			}, &sec)
			if err != nil {
				if apierrors.IsNotFound(err) {
					// User referenced a Secret that doesn't exist yet
					// (e.g., cert-manager hasn't issued it). Skip
					// silently — the eventual Secret create event will
					// re-fire reconcile and materialize it then.
					continue
				}
				if firstErr == nil {
					firstErr = fmt.Errorf("get secret %s: %w", key, err)
				}
				continue
			}
			if err := writeTLSFiles(r.TLSCertDir, ing.Namespace, t.SecretName, &sec); err != nil {
				if firstErr == nil {
					firstErr = err
				}
			}
		}
	}

	// Prune stale files. Conservative scope: only *.crt / *.key
	// files matching our naming convention. Don't touch anything
	// else under TLSCertDir — operators might mount something else
	// in there (CA bundles, etc.).
	if err := r.pruneStaleTLS(keep); err != nil && firstErr == nil {
		firstErr = err
	}

	return firstErr
}

// pruneStaleTLS deletes *.crt / *.key files under TLSCertDir that
// aren't in `keep`. The keep set was assembled from the current
// Ingress list, so anything missing is genuinely orphaned material
// from a previously-referenced Secret.
func (r *Reconciler) pruneStaleTLS(keep map[string]struct{}) error {
	entries, err := os.ReadDir(r.TLSCertDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // dir doesn't exist yet, nothing to prune
		}
		return fmt.Errorf("read %s: %w", r.TLSCertDir, err)
	}
	var firstErr error
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".crt") && !strings.HasSuffix(name, ".key") {
			continue
		}
		if _, want := keep[name]; want {
			continue
		}
		path := filepath.Join(r.TLSCertDir, name)
		if rmErr := os.Remove(path); rmErr != nil && firstErr == nil {
			firstErr = fmt.Errorf("prune %s: %w", path, rmErr)
		}
	}
	return firstErr
}

// buildServicePortMap walks the Services referenced by `ours` and
// returns a "<ns>/<svc>/<portName>" → portNumber map for the
// translator's named-port resolution. Only Services actually used by
// an Ingress are looked up — we don't scan the whole cluster.
//
// Missing Services are skipped silently; the eventual Service create
// event re-fires reconcile and populates the map then.
func (r *Reconciler) buildServicePortMap(ctx context.Context, ours []networkingv1.Ingress) (map[string]int32, error) {
	out := map[string]int32{}
	seen := map[string]struct{}{}
	var firstErr error
	for _, ing := range ours {
		for _, rule := range ing.Spec.Rules {
			if rule.HTTP == nil {
				continue
			}
			for _, p := range rule.HTTP.Paths {
				if p.Backend.Service == nil || p.Backend.Service.Port.Name == "" {
					continue
				}
				key := ing.Namespace + "/" + p.Backend.Service.Name
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				var svc corev1.Service
				err := r.Get(ctx, types.NamespacedName{
					Namespace: ing.Namespace,
					Name:      p.Backend.Service.Name,
				}, &svc)
				if err != nil {
					if apierrors.IsNotFound(err) {
						continue
					}
					if firstErr == nil {
						firstErr = fmt.Errorf("get service %s: %w", key, err)
					}
					continue
				}
				for _, sp := range svc.Spec.Ports {
					if sp.Name == "" {
						continue
					}
					out[ing.Namespace+"/"+svc.Name+"/"+sp.Name] = sp.Port
				}
			}
		}
	}
	return out, firstErr
}

func writeTLSFiles(dir, ns, name string, sec *corev1.Secret) error {
	crt := sec.Data[corev1.TLSCertKey]
	key := sec.Data[corev1.TLSPrivateKeyKey]
	if len(crt) == 0 || len(key) == 0 {
		return fmt.Errorf("secret %s/%s missing tls.crt or tls.key", ns, name)
	}
	base := strings.TrimRight(dir, "/")
	pairs := []struct {
		path string
		data []byte
		mode os.FileMode
	}{
		{filepath.Join(base, ns+"-"+name+".crt"), crt, 0o644},
		// 0640 on the key — nvelox container's user reads it; not
		// world-readable.
		{filepath.Join(base, ns+"-"+name+".key"), key, 0o640},
	}
	for _, p := range pairs {
		if err := atomicWrite(p.path, p.data, p.mode); err != nil {
			return fmt.Errorf("write %s: %w", p.path, err)
		}
	}
	return nil
}

// atomicWrite renames a temp file into place so a concurrent nvelox
// read never sees a partial file. Same pattern as reloader.Apply.
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tls-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}
