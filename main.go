// nvelox-ingress-controller — watches Kubernetes Ingress resources
// whose ingressClassName matches our class, translates them into
// nvelox YAML, and signals an nvelox sidecar to hot-reload.
//
// Designed to run alongside the nvelox process in the SAME pod with
// shareProcessNamespace=true, so SIGHUP works without leaving the
// pod. Service-of-record exposes the nvelox container's listener
// ports (default 8080 / 8443) to the cluster.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/nvelox/nvelox-ingress-controller/internal/ingress"
	"github.com/nvelox/nvelox-ingress-controller/internal/reloader"
	"github.com/nvelox/nvelox-ingress-controller/internal/translator"
)

// seedInitialConfig renders the controller's "empty cluster" view
// (zero Ingresses) and writes it before the manager starts watching.
// Required because controller-runtime informers don't emit any event
// when the watched list is empty, so without this the first reconcile
// never fires on a fresh cluster — nvelox would sit unbound on the
// listener ports until someone applied an Ingress.
//
// With DefaultBackendRoot set, the translator emits the catch-all 404
// listener even with no Ingresses, which is what makes the port bind.
func seedInitialConfig(rel *reloader.Reloader, httpPort, httpsPort int, tlsCertDir, defaultRoot string, trustedProxies []string) error {
	data, err := translator.Render(translator.Inputs{
		HTTPPort:           httpPort,
		HTTPSPort:          httpsPort,
		TLSCertDir:         tlsCertDir,
		DefaultBackendRoot: defaultRoot,
		TrustedProxies:     trustedProxies,
	})
	if err != nil {
		return err
	}
	_, err = rel.Apply(data)
	return err
}

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(networkingv1.AddToScheme(scheme))
}

func main() {
	var (
		ingressClass       string
		configPath         string
		pidFile            string
		procName           string
		tlsCertDir         string
		defaultBackendRoot string
		publishService     string
		trustedProxiesRaw  string
		httpPort           int
		httpsPort          int
		nveloxWait         time.Duration
		metricsAddr        string
		probeAddr          string
		leaderElect        bool
		leaderElectionID   string
	)
	flag.StringVar(&ingressClass, "ingress-class", envOr("INGRESS_CLASS", "nvelox"),
		"IngressClass name we own. Ingresses with spec.ingressClassName matching this are reconciled.")
	flag.StringVar(&configPath, "config-path", envOr("CONFIG_PATH", "/etc/nvelox/conf.d/k8s.yaml"),
		"Path the rendered nvelox YAML is written to. Must be in a directory the nvelox sidecar `include`s.")
	flag.StringVar(&pidFile, "pid-file", envOr("PID_FILE", "/var/run/nvelox/nvelox.pid"),
		"PID file written by nvelox; controller reads it to find the SIGHUP target.")
	flag.StringVar(&procName, "proc-name", envOr("PROC_NAME", "nvelox"),
		"Process name fallback when pid_file is missing (e.g. during nvelox startup).")
	flag.StringVar(&tlsCertDir, "tls-cert-dir", envOr("TLS_CERT_DIR", "/etc/nvelox/tls"),
		"Directory where the controller materializes Secret.tls.crt/tls.key for nvelox to read.")
	flag.StringVar(&defaultBackendRoot, "default-backend-root", envOr("DEFAULT_BACKEND_ROOT", "/etc/nvelox/default-www"),
		"Path nvelox's static catch-all uses as root. Must point at an empty directory mounted into the nvelox container — every $uri misses, fallback returns 404. Set to empty string to disable the catch-all (port refuses connections until first Ingress).")
	flag.StringVar(&trustedProxiesRaw, "trusted-proxies", envOr("TRUSTED_PROXIES", ""),
		"Comma-separated CIDRs/IPs emitted as `trusted_proxies` on every generated nvelox listener. Set this to the upstream proxy's source range (e.g. an edge GW nvelox + pod/node CIDRs) when this nvelox runs BEHIND another proxy, so it appends to X-Forwarded-For instead of overwriting it — required for the real client IP to survive the hop. Empty = trust no upstream (XFF replaced with peer).")
	flag.StringVar(&publishService, "publish-service", envOr("PUBLISH_SERVICE", ""),
		"Service (form: <namespace>/<name>) whose external address gets written back to every owned Ingress's status.loadBalancer.ingress[]. LoadBalancer → uses .status.loadBalancer; NodePort → uses Node InternalIPs; ClusterIP / empty → no status updates.")
	flag.IntVar(&httpPort, "http-port", envIntOr("HTTP_PORT", 8080),
		"Pod-internal HTTP listener port nvelox binds to. Service in front maps :80 → this.")
	flag.IntVar(&httpsPort, "https-port", envIntOr("HTTPS_PORT", 8443),
		"Pod-internal HTTPS listener port nvelox binds to. Service in front maps :443 → this.")
	flag.DurationVar(&nveloxWait, "nvelox-wait", 30*time.Second,
		"How long to wait at boot for the nvelox sidecar process to appear before the first reconcile.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8082",
		"controller-runtime metrics endpoint.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8083",
		"healthz / readyz probe endpoint.")
	flag.BoolVar(&leaderElect, "leader-elect", false,
		"Enable leader election. Required when running >1 replica.")
	flag.StringVar(&leaderElectionID, "leader-elect-id", "nvelox-ingress-controller",
		"Lease name used for leader election.")
	flag.Parse()

	// Split the comma-separated --trusted-proxies into a clean slice
	// (trim spaces, drop empties) — shared by the seed render and the
	// reconciler so both emit the same trusted_proxies on every listener.
	trustedProxies := splitCSV(trustedProxiesRaw)

	// One JSON handler drives both stacks:
	//   - slog.Default()       — our own log lines (slog.Info / Warn / Error)
	//   - controller-runtime   — reconcile errors, leader-election events,
	//                            watch failures
	// Without ctrl.SetLogger(...) controller-runtime drops all its
	// internal log lines and prints a one-time stack-trace nag
	// ("log.SetLogger(...) was never called; logs will not be displayed")
	// the first time something tries to log.
	logHandler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(logHandler))
	ctrl.SetLogger(logr.FromSlogHandler(logHandler))

	rel := &reloader.Reloader{
		ConfigPath: configPath,
		PIDFile:    pidFile,
		ProcName:   procName,
	}

	// Hold the first reconcile until nvelox is alive so the initial
	// SIGHUP lands. A failure here is non-fatal: the controller still
	// boots, writes the config on the first event, and the eventual
	// natural-startup nvelox will pick it up.
	if err := rel.WaitForNvelox(nveloxWait); err != nil {
		slog.Warn("nvelox not detected at boot; reconciling anyway", "err", err)
	}

	// Seed the initial config so the listener binds before the first
	// informer event. With zero Ingresses the render is just the
	// catch-all 404 listener (when DefaultBackendRoot is set); with
	// it empty, the render is empty and nvelox stays unbound until
	// the first Ingress.
	if err := seedInitialConfig(rel, httpPort, httpsPort, tlsCertDir, defaultBackendRoot, trustedProxies); err != nil {
		slog.Warn("initial seed failed; first Ingress event will write the config", "err", err)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), manager.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       leaderElectionID,
	})
	if err != nil {
		slog.Error("manager start failed", "err", err)
		os.Exit(1)
	}

	r := &ingress.Reconciler{
		Client:             mgr.GetClient(),
		IngressClass:       ingressClass,
		HTTPPort:           httpPort,
		HTTPSPort:          httpsPort,
		TLSCertDir:         tlsCertDir,
		DefaultBackendRoot: defaultBackendRoot,
		PublishService:     publishService,
		TrustedProxies:     trustedProxies,
		Reload:             rel,
	}

	// Watch Ingress as primary. Secondary watches:
	//   * Secrets — kubernetes.io/tls update (cert rotation,
	//     cert-manager renewal) re-renders without waiting for an
	//     Ingress event.
	//   * Services — a Service's port name/number change has to
	//     re-fire reconcile so the named-port resolution map stays
	//     in sync. Without this watch, renaming a port silently
	//     breaks routing until the next Ingress edit.
	//
	// All watches enqueue the SAME synthetic key — Reconcile rebuilds
	// the whole world on each fire, so the exact triggering object
	// doesn't matter.
	if err := builder.ControllerManagedBy(mgr).
		For(&networkingv1.Ingress{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(secretToRequest)).
		Watches(&corev1.Service{}, handler.EnqueueRequestsFromMapFunc(secretToRequest)).
		// EndpointSlices feed the per-pod backend resolution
		// (#210). Pod adds/removes / readiness flips re-fire
		// reconcile so nvelox sees the latest pod list.
		Watches(&discoveryv1.EndpointSlice{}, handler.EnqueueRequestsFromMapFunc(secretToRequest)).
		Complete(r); err != nil {
		slog.Error("controller setup failed", "err", err)
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", func(_ *http.Request) error { return nil }); err != nil {
		slog.Warn("add healthz failed", "err", err)
	}
	if err := mgr.AddReadyzCheck("readyz", func(_ *http.Request) error { return nil }); err != nil {
		slog.Warn("add readyz failed", "err", err)
	}

	slog.Info("nvelox-ingress-controller starting",
		"ingress_class", ingressClass,
		"config_path", configPath,
		"http_port", httpPort,
		"https_port", httpsPort,
		"leader_elect", leaderElect,
	)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		slog.Error("manager exited", "err", err)
		os.Exit(1)
	}
}

// secretToRequest turns a Secret change into a reconcile request. The
// reconciler ignores the specific NamespacedName (it rebuilds the
// whole config), so we just emit one stable synthetic key.
func secretToRequest(_ context.Context, _ client.Object) []reconcile.Request {
	return []reconcile.Request{{}}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// splitCSV parses a comma-separated list into a trimmed, empty-free
// slice. Returns nil for an empty/whitespace input so downstream
// `omitempty` drops the field entirely.
func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envIntOr(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	var out int
	if _, err := fmt.Sscanf(v, "%d", &out); err != nil {
		return def
	}
	return out
}
