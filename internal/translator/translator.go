// Package translator converts a set of Kubernetes Ingress objects into
// the nvelox YAML config format. It is intentionally pure: takes data
// in, returns bytes out — no Kubernetes client, no filesystem, no
// reload trigger. That makes it trivially unit-testable and keeps the
// reconciler thin.
//
// Coverage today:
//   - HTTP ingresses (host + path-prefix routing)
//   - HTTPS terminated at nvelox when Ingress.Spec.TLS is set; the
//     reconciler is responsible for materializing the cert/key files
//     into the shared volume the rendered YAML points at.
//   - Backend resolution uses the Service's in-cluster DNS name
//     (svc.ns.svc.cluster.local:port). Cheap, works through kube-proxy,
//     no Endpoints watch needed for v1.
//
// Not yet wired (deliberate v1 scope cut):
//   - TCPRoute / UDPRoute / Gateway API
//   - Path types other than Prefix / ImplementationSpecific
//   - IngressClass parameters (nvelox-specific tuning per class)
//   - Sticky sessions / per-route middleware
package translator

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	"github.com/nvelox/nvelox-ingress-controller/internal/annotations"
)

// Inputs is the snapshot the reconciler hands the translator on every
// change. Ordering matters for deterministic output (and therefore
// deterministic reload no-ops) — the translator sorts what it needs.
type Inputs struct {
	// Ingresses are the resources we own (already filtered by
	// IngressClass upstream). Translator does no further filtering.
	Ingresses []networkingv1.Ingress

	// TLSCertDir is the path inside the nvelox container where the
	// reconciler will have written `<secret-namespace>-<secret-name>.crt`
	// and `.key` for every Secret referenced by spec.tls. The
	// translator just builds the path strings.
	TLSCertDir string

	// HTTPPort / HTTPSPort are what the rendered listeners bind to
	// INSIDE the pod. The Service in front of nvelox maps real
	// 80/443 traffic onto these — keep them as values to allow
	// non-privileged runs.
	HTTPPort  int
	HTTPSPort int

	// DefaultBackendRoot, when non-empty, makes the renderer always
	// emit the HTTP listener with a permanent catch-all route that
	// serves nvelox's `static` + `try_files` from the given root,
	// falling back to a 404. The chart points this at an emptyDir
	// mount so the static directory is guaranteed empty — every
	// request misses every $uri and lands on the fallback.
	//
	// Empty disables the catch-all (listener only emitted when there
	// are real routes; port refuses connections on a fresh cluster).
	DefaultBackendRoot string

	// ServicePorts resolves named Ingress ports (backend.service.port.name)
	// to their numeric counterpart. Key shape: "<namespace>/<service>/<portName>".
	// Reconciler builds this by listing Services in the cluster on each
	// reconcile. When the map is empty or the key is absent, named-port
	// routes are dropped silently — same as the v1 "named ports not
	// supported" behavior. That keeps the contract additive: deploys
	// that don't wire ServicePorts keep working unchanged.
	ServicePorts map[string]int32

	// EndpointAddresses, when set, provides per-pod backend addresses
	// keyed by "<namespace>/<service>/<port>" → ["10.1.2.3:5678",
	// "10.1.2.4:5678", ...]. When present, the translator emits one
	// `backend.servers` entry per pod IP instead of the Service DNS
	// fallback. This bypasses kube-proxy and unlocks nvelox's L4
	// strategies (least-conn, sticky-IP-hash, real per-pod sticky
	// cookie). When absent or empty for a key, the translator falls
	// back to the Service DNS form — backward compatible.
	//
	// Reconciler builds this from discovery.k8s.io/v1 EndpointSlices.
	EndpointAddresses map[string][]string

	// AnnotationOverrides, when set, replaces the Spec the translator
	// would derive from annotations.Parse for a given Ingress. Key
	// shape: "<namespace>/<name>". Used by the reconciler to merge
	// IngressClass parameter defaults (#213) under per-Ingress
	// annotations before handing off to the translator — keeps the
	// translator unaware of class-level config.
	AnnotationOverrides map[string]annotations.Spec
}

// nveloxConfig is the minimal slice of nvelox.yaml we render. Extra
// fields (logging, metrics, admin, etc.) live in the base ConfigMap;
// our output is `include`-d alongside it.
type nveloxConfig struct {
	Listeners []listener `yaml:"listeners,omitempty" json:"listeners,omitempty"`
	Backends  []backend  `yaml:"backends,omitempty" json:"backends,omitempty"`
}

type listener struct {
	Name          string             `yaml:"name" json:"name"`
	Bind          string             `yaml:"bind" json:"bind"`
	Protocol      string             `yaml:"protocol" json:"protocol"`
	ServerNames   []string           `yaml:"server_names,omitempty" json:"server_names,omitempty"`
	DefaultServer bool               `yaml:"default_server,omitempty" json:"default_server,omitempty"`
	TLS           *tlsBlock          `yaml:"tls,omitempty" json:"tls,omitempty"`
	IPRateLimit   *ipRateLimitBlock  `yaml:"ip_rate_limit,omitempty" json:"ip_rate_limit,omitempty"`
	IPAllowlist   []string           `yaml:"ip_allowlist,omitempty" json:"ip_allowlist,omitempty"`
	IPDenylist    []string           `yaml:"ip_denylist,omitempty" json:"ip_denylist,omitempty"`
	Routes        []route            `yaml:"routes,omitempty" json:"routes,omitempty"`
}

// ipRateLimitBlock models nvelox's listener-level per-client-IP
// request limit (see nvelox.example.yaml:122-124). Both fields are
// required when the block is set — the translator only emits this
// block when an annotation actually requested a limit.
type ipRateLimitBlock struct {
	RequestsPerSecond int `yaml:"requests_per_second" json:"requests_per_second"`
	Burst             int `yaml:"burst" json:"burst"`
}

// tlsBlock models nvelox's listener `tls:` block. Field names are
// `cert` and `key` per nvelox.example.yaml:84-86 — NOT `cert_file` /
// `key_file`. nvelox treats the wrong field name as "absent" and
// rejects the listener with "HTTPS listener requires TLS cert/key".
// Don't reintroduce the *_file suffixes.
type tlsBlock struct {
	Cert string `yaml:"cert" json:"cert"`
	Key  string `yaml:"key" json:"key"`
}

type route struct {
	Match    matchBlock      `yaml:"match" json:"match"`
	Backend  string          `yaml:"backend,omitempty" json:"backend,omitempty"`
	Static   *staticBlock    `yaml:"static,omitempty" json:"static,omitempty"`
	TryFiles *tryFilesBlock  `yaml:"try_files,omitempty" json:"try_files,omitempty"`
	Redirect *redirectBlock  `yaml:"redirect,omitempty" json:"redirect,omitempty"`
	Rewrite  *rewriteBlock   `yaml:"rewrite,omitempty" json:"rewrite,omitempty"`
	Headers  *headersBlock   `yaml:"headers,omitempty" json:"headers,omitempty"`
}

// redirectBlock matches nvelox's `redirect:` route action shape (see
// nvelox.example.yaml, the http-redirect listener example). url
// supports the ${host} ${uri} ${scheme} variables nvelox expands at
// request time, so a single static template works across every host.
type redirectBlock struct {
	URL  string `yaml:"url" json:"url"`
	Code int    `yaml:"code" json:"code"`
}

// rewriteBlock matches nvelox's `rewrite:` route action (see
// nvelox.example.yaml:176-177). path supports `$N` references to
// capture groups from the route's path_regex match.
type rewriteBlock struct {
	Path string `yaml:"path" json:"path"`
}

// headersBlock injects headers on the request and/or response paths
// of the route. Maps to nvelox's `headers.{request_add, response_add}`
// blocks.
type headersBlock struct {
	RequestAdd  map[string]string `yaml:"request_add,omitempty" json:"request_add,omitempty"`
	ResponseAdd map[string]string `yaml:"response_add,omitempty" json:"response_add,omitempty"`
}

type matchBlock struct {
	Host       string `yaml:"host,omitempty" json:"host,omitempty"`
	PathPrefix string `yaml:"path_prefix,omitempty" json:"path_prefix,omitempty"`
	// PathRegex is mutually exclusive with PathPrefix — only set
	// when the route needs capture groups (e.g., strip-prefix).
	PathRegex  string `yaml:"path_regex,omitempty" json:"path_regex,omitempty"`
}

// staticBlock + tryFilesBlock model nvelox's static-file route shape.
// We use them only for the always-on catch-all 404 — root points at an
// empty emptyDir mount, files: ["$uri"] always misses, fallback fires.
type staticBlock struct {
	Root string `yaml:"root" json:"root"`
}

type tryFilesBlock struct {
	Files    []string `yaml:"files" json:"files"`
	Fallback string   `yaml:"fallback" json:"fallback"`
}

// backend models nvelox's `backends:` entry. servers MUST be a list
// of raw "host:port" strings (see nvelox.example.yaml:324). An earlier
// version used []struct{Addr string} which serialized to maps and
// tripped nvelox's pre-flight validator: "cannot unmarshal !!map into
// string". Don't reintroduce the struct form.
type backend struct {
	Name          string         `yaml:"name" json:"name"`
	Servers       []string       `yaml:"servers" json:"servers"`
	StickySession *stickyBlock   `yaml:"sticky_session,omitempty" json:"sticky_session,omitempty"`
}

// stickyBlock models nvelox's backend-level session affinity (see
// nvelox.example.yaml:284-287). Type is always "cookie" for the
// nvelox.io/sticky-cookie annotation; header / ip_hash variants
// would each warrant their own annotation if/when we add them.
// TTL defaults to "1h" to match the canonical example.
type stickyBlock struct {
	Type       string `yaml:"type" json:"type"`
	CookieName string `yaml:"cookie_name" json:"cookie_name"`
	TTL        string `yaml:"ttl" json:"ttl"`
}

// Render walks the Ingresses and produces the nvelox YAML fragment.
// Output is deterministic: identical inputs → byte-identical output,
// so the reconciler's diff-then-reload path skips no-op writes.
func Render(in Inputs) ([]byte, error) {
	if in.HTTPPort == 0 {
		in.HTTPPort = 8080
	}
	if in.HTTPSPort == 0 {
		in.HTTPSPort = 8443
	}
	if in.TLSCertDir == "" {
		in.TLSCertDir = "/etc/nvelox/tls"
	}

	// Sort ingresses so we walk them in a stable order regardless of
	// the informer's iteration order on a given reconcile.
	sort.Slice(in.Ingresses, func(i, j int) bool {
		a, b := in.Ingresses[i], in.Ingresses[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})

	backends := map[string]backend{}
	httpRoutes := []route{}
	// HTTPS terminations: one listener per (host, tls-secret) tuple so
	// each site presents its own cert. server_names keeps a single
	// :443 bind shared across hosts via SNI.
	httpsListeners := map[string]*listener{}
	// Most-restrictive (rps, burst) per listener-key. "k8s-http" for
	// the shared HTTP listener, the httpsListeners map key for each
	// HTTPS listener. 0 means "no limit set". applyRateLimit merges
	// in the most-restrictive value, ignoring 0s.
	// First-Ingress-wins per backend. Multiple Ingresses pointing at
	// the same Service-port collapse to one backend in our render;
	// if they disagree on sticky-cookie name, we pick the first one
	// the iteration encounters. Stable thanks to the ingress sort.
	stickyCookies := map[string]string{}
	// Per-listener CIDR filters. UNION across contributing Ingresses
	// so denies stay additive (every contributor's denies apply) and
	// allowlists keep parity. Use a dedupe set so the same CIDR
	// declared on multiple Ingresses appears once in the output.
	allowCIDRs := map[string]map[string]struct{}{} // listener-key → set of cidr
	denyCIDRs := map[string]map[string]struct{}{}
	applyCIDRs := func(dst map[string]map[string]struct{}, lkey string, cidrs []string) {
		if len(cidrs) == 0 {
			return
		}
		s, ok := dst[lkey]
		if !ok {
			s = map[string]struct{}{}
			dst[lkey] = s
		}
		for _, c := range cidrs {
			s[c] = struct{}{}
		}
	}

	rateLimits := map[string]struct{ rps, burst int }{}
	applyRateLimit := func(lkey string, rps, burst int) {
		if rps <= 0 {
			return
		}
		cur, ok := rateLimits[lkey]
		if !ok || rps < cur.rps {
			cur.rps = rps
		}
		// burst: take the SMALLER non-zero burst too — matches the
		// "most restrictive wins" semantics of the rps choice. An
		// empty burst means "no contributor set one yet", in which
		// case adopt this value outright.
		if !ok || cur.burst == 0 || (burst > 0 && burst < cur.burst) {
			cur.burst = burst
		}
		rateLimits[lkey] = cur
	}

	for _, ing := range in.Ingresses {
		// Map host → tls secret (if any) for this ingress. spec.tls[]
		// entries may list multiple hosts per secret.
		hostTLS := map[string]types.NamespacedName{}
		for _, t := range ing.Spec.TLS {
			if t.SecretName == "" {
				continue
			}
			for _, h := range t.Hosts {
				hostTLS[strings.ToLower(h)] = types.NamespacedName{
					Namespace: ing.Namespace,
					Name:      t.SecretName,
				}
			}
		}

		// Per-Ingress annotation Spec. When the reconciler has
		// pre-merged class defaults under the annotations (#213),
		// it passes the result in via AnnotationOverrides. We
		// trust that override and skip re-parsing. Otherwise fall
		// back to parsing the Ingress directly — keeps standalone
		// translator users (tests, render_check.go) working without
		// having to wire a parameters layer.
		var ann annotations.Spec
		if in.AnnotationOverrides != nil {
			if s, ok := in.AnnotationOverrides[ing.Namespace+"/"+ing.Name]; ok {
				ann = s
			} else {
				ann = annotations.Parse(&ing)
			}
		} else {
			ann = annotations.Parse(&ing)
		}
		// Dedupe redirect-https emissions per host — multiple paths
		// under one host should still produce ONE redirect route
		// (otherwise nvelox would have duplicate match rules).
		redirectEmitted := map[string]bool{}

		// rateLimitFromAnn returns the per-second-equivalent budget
		// the annotations request, or 0 if no limit was set. When
		// both per-second and per-minute are set, the smaller (more
		// restrictive) wins. burst follows the same rule.
		annRPS, annBurst := rateLimitFromAnn(ann)

		for _, rule := range ing.Spec.Rules {
			host := strings.ToLower(strings.TrimSpace(rule.Host))
			if rule.HTTP == nil {
				continue
			}
			for _, p := range rule.HTTP.Paths {
				if p.Backend.Service == nil {
					// Resource-typed backends (ObjectRef) aren't
					// supported in v1 — they need provider-specific
					// resolution which we'd plumb via a CRD later.
					continue
				}
				port := servicePort(p.Backend.Service, ing.Namespace, in.ServicePorts)
				if port == 0 {
					continue
				}
				bName := backendName(ing.Namespace, p.Backend.Service.Name, port)
				if _, seen := backends[bName]; !seen {
					// Prefer per-pod addresses from EndpointSlices.
					// Falls back to the Service DNS form when no
					// endpoint info is available — this keeps deploys
					// that skip the EndpointSlices informer working,
					// and matches behavior for pre-Ready services
					// (no endpoints yet).
					key := fmt.Sprintf("%s/%s/%d", ing.Namespace, p.Backend.Service.Name, port)
					servers := in.EndpointAddresses[key]
					if len(servers) == 0 {
						servers = []string{
							fmt.Sprintf("%s.%s.svc.cluster.local:%d",
								p.Backend.Service.Name, ing.Namespace, port),
						}
					}
					backends[bName] = backend{
						Name:    bName,
						Servers: servers,
					}
				}
				// First Ingress with a sticky-cookie annotation wins
				// for this backend. Multiple Ingresses pointing at the
				// same backend should agree on stickiness; if they
				// disagree, the iteration order (stable thanks to
				// the ingress sort above) decides.
				if ann.StickyCookie != "" {
					if _, ok := stickyCookies[bName]; !ok {
						stickyCookies[bName] = ann.StickyCookie
					}
				}

				prefix := pathPrefixFor(p)
				r := route{
					Match:   matchBlock{Host: host, PathPrefix: prefix},
					Backend: bName,
				}

				// strip-prefix: switch to regex match + emit rewrite.
				// Only when the annotation's prefix is actually a
				// prefix of this path — otherwise the strip would be
				// a no-op and the regex would also restrict matches
				// unnecessarily.
				if ann.StripPrefix != "" && strings.HasPrefix(prefix, ann.StripPrefix) {
					r.Match.PathPrefix = ""
					// Anchor on the original path; capture the rest.
					r.Match.PathRegex = "^" + regexp.QuoteMeta(prefix) + "(.*)$"
					rewritten := strings.TrimPrefix(prefix, ann.StripPrefix)
					// When the strip equals the path exactly, leave
					// the rewrite to the capture alone — nvelox treats
					// an empty path as "/". Otherwise concat the
					// surviving prefix with the captured suffix.
					if rewritten == "" {
						r.Rewrite = &rewriteBlock{Path: "$1"}
					} else {
						r.Rewrite = &rewriteBlock{Path: rewritten + "$1"}
					}
				}

				// Request / response header injection.
				if len(ann.RequestHeaders) > 0 || len(ann.ResponseHeaders) > 0 {
					r.Headers = &headersBlock{
						RequestAdd:  ann.RequestHeaders,
						ResponseAdd: ann.ResponseHeaders,
					}
				}

				if secret, ok := hostTLS[host]; ok {
					key := host + "|" + secret.String()
					l, exists := httpsListeners[key]
					if !exists {
						l = &listener{
							Name:        listenerName("https", ing.Namespace, ing.Name, host),
							Bind:        fmt.Sprintf(":%d", in.HTTPSPort),
							Protocol:    "https",
							ServerNames: []string{host},
							TLS: &tlsBlock{
								Cert: fmt.Sprintf("%s/%s-%s.crt",
									strings.TrimRight(in.TLSCertDir, "/"),
									secret.Namespace, secret.Name),
								Key: fmt.Sprintf("%s/%s-%s.key",
									strings.TrimRight(in.TLSCertDir, "/"),
									secret.Namespace, secret.Name),
							},
						}
						httpsListeners[key] = l
					}
					l.Routes = append(l.Routes, r)
					applyRateLimit(key, annRPS, annBurst)
					applyCIDRs(allowCIDRs, key, ann.AllowCIDRs)
					applyCIDRs(denyCIDRs, key, ann.DenyCIDRs)

					// Emit a 301 HTTP→HTTPS redirect on the HTTP
					// listener for this host. Only fires when (a) the
					// Ingress opted in via the annotation, (b) the
					// host actually has a TLS entry (otherwise we'd
					// redirect to nothing), and (c) we haven't
					// already emitted one for this host.
					if ann.RedirectHTTPS && host != "" && !redirectEmitted[host] {
						httpRoutes = append(httpRoutes, route{
							Match: matchBlock{Host: host, PathPrefix: "/"},
							Redirect: &redirectBlock{
								URL:  "https://${host}${uri}",
								Code: 301,
							},
						})
						redirectEmitted[host] = true
					}
				} else {
					httpRoutes = append(httpRoutes, r)
					applyRateLimit("k8s-http", annRPS, annBurst)
					applyCIDRs(allowCIDRs, "k8s-http", ann.AllowCIDRs)
					applyCIDRs(denyCIDRs, "k8s-http", ann.DenyCIDRs)
				}
			}
		}
	}

	cfg := nveloxConfig{}
	// Catch-all 404 (when DefaultBackendRoot is set) goes LAST so
	// first-match-wins ordering means real Ingress routes still win.
	// nvelox rejects a listener with no routes ("requires 'backend'
	// or 'routes'") — appending this fallback keeps the listener
	// valid even on a fresh cluster with zero Ingresses, so the
	// in-pod port is bound from the moment nvelox boots and clients
	// get a clean 404 instead of "connection refused".
	if in.DefaultBackendRoot != "" {
		httpRoutes = append(httpRoutes, route{
			Match: matchBlock{PathPrefix: "/"},
			Static: &staticBlock{
				Root: strings.TrimRight(in.DefaultBackendRoot, "/"),
			},
			TryFiles: &tryFilesBlock{
				Files:    []string{"$uri"},
				Fallback: "=404",
			},
		})
	}
	if len(httpRoutes) > 0 {
		l := listener{
			Name:     "k8s-http",
			Bind:     fmt.Sprintf(":%d", in.HTTPPort),
			Protocol: "http",
			Routes:   httpRoutes,
		}
		if rl, ok := rateLimits["k8s-http"]; ok && rl.rps > 0 {
			l.IPRateLimit = &ipRateLimitBlock{
				RequestsPerSecond: rl.rps,
				Burst:             rl.burst,
			}
		}
		l.IPAllowlist = sortedCIDRs(allowCIDRs["k8s-http"])
		l.IPDenylist = sortedCIDRs(denyCIDRs["k8s-http"])
		cfg.Listeners = append(cfg.Listeners, l)
	}
	// Stable order for the per-host HTTPS listeners.
	keys := make([]string, 0, len(httpsListeners))
	for k := range httpsListeners {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		l := httpsListeners[k]
		if rl, ok := rateLimits[k]; ok && rl.rps > 0 {
			l.IPRateLimit = &ipRateLimitBlock{
				RequestsPerSecond: rl.rps,
				Burst:             rl.burst,
			}
		}
		l.IPAllowlist = sortedCIDRs(allowCIDRs[k])
		l.IPDenylist = sortedCIDRs(denyCIDRs[k])
		cfg.Listeners = append(cfg.Listeners, *l)
	}

	// Backends in stable order. Sticky-cookie is attached here, at
	// emit time, so we have the complete first-wins view across all
	// Ingresses (some of which may have added entries to stickyCookies
	// AFTER the backend was first created).
	bnames := make([]string, 0, len(backends))
	for n := range backends {
		bnames = append(bnames, n)
	}
	sort.Strings(bnames)
	for _, n := range bnames {
		b := backends[n]
		if cookie, ok := stickyCookies[n]; ok && cookie != "" {
			b.StickySession = &stickyBlock{
				Type:       "cookie",
				CookieName: cookie,
				TTL:        "1h",
			}
		}
		cfg.Backends = append(cfg.Backends, b)
	}

	// sigs.k8s.io/yaml gives us encoding/json's struct tag semantics
	// while emitting YAML — matches the json tags above and avoids the
	// quirks of gopkg.in/yaml.v3's mapstructure-style encoding.
	return yaml.Marshal(cfg)
}

// servicePort resolves an Ingress backend port to its numeric form.
// Number takes precedence (explicit user choice). When only a Name is
// set, we look it up in the resolver map the reconciler built from
// the in-cluster Services. Missing key → 0 → route is dropped by the
// caller (silent skip — same contract as a missing Service).
// sortedCIDRs flattens a dedupe-set into a stable-sorted []string for
// rendering. Returns nil for empty input so the listener's yaml omits
// the empty list entirely (omitempty struct tag does the rest).
func sortedCIDRs(set map[string]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// rateLimitFromAnn folds the per-second + per-minute annotations into
// a single (rps, burst) pair the translator emits as the listener's
// ip_rate_limit block. Rules:
//   - Either / both / neither annotation may be set.
//   - per-minute is converted to RPS via ceil-division (so "30/min"
//     becomes 1 rps, not 0; nvelox doesn't accept fractional rps).
//   - When both are set, the more restrictive RPS wins.
//   - Burst defaults to the per-minute value when minute-only (allows
//     bursting a full minute's worth before throttling kicks in), to
//     the per-second value when second-only, to the smaller of the
//     two when both. burst==rps as a default is intentionally
//     conservative — operators who want more headroom can set their
//     annotations larger.
//
// Returns (0, 0) when no annotation was set — caller treats that as
// "no rate-limit block emitted".
func rateLimitFromAnn(a annotations.Spec) (rps, burst int) {
	rpsFromMinute := 0
	if a.RateLimitPerMinute > 0 {
		// ceil(N/60). Done with integer math so the package stays
		// stdlib-free.
		rpsFromMinute = (a.RateLimitPerMinute + 59) / 60
		if rpsFromMinute < 1 {
			rpsFromMinute = 1
		}
	}

	switch {
	case a.RateLimitPerSecond > 0 && rpsFromMinute > 0:
		// Both annotations set — the smaller rps is the binding
		// constraint. Burst follows whichever annotation drove the
		// rps choice so the two stay coherent.
		if a.RateLimitPerSecond <= rpsFromMinute {
			return a.RateLimitPerSecond, a.RateLimitPerSecond
		}
		return rpsFromMinute, a.RateLimitPerMinute
	case a.RateLimitPerSecond > 0:
		return a.RateLimitPerSecond, a.RateLimitPerSecond
	case rpsFromMinute > 0:
		return rpsFromMinute, a.RateLimitPerMinute
	default:
		return 0, 0
	}
}

func servicePort(b *networkingv1.IngressServiceBackend, ns string, ports map[string]int32) int {
	if b == nil {
		return 0
	}
	if b.Port.Number > 0 {
		return int(b.Port.Number)
	}
	if b.Port.Name != "" && ports != nil {
		if num, ok := ports[ns+"/"+b.Name+"/"+b.Port.Name]; ok && num > 0 {
			return int(num)
		}
	}
	return 0
}

func pathPrefixFor(p networkingv1.HTTPIngressPath) string {
	prefix := p.Path
	if prefix == "" {
		prefix = "/"
	}
	// PathType: only Prefix and ImplementationSpecific map cleanly to
	// nvelox's path_prefix today. Exact + regex paths need different
	// match blocks — defer until we add them to the YAML schema mapping.
	if p.PathType != nil && *p.PathType == networkingv1.PathTypeExact {
		// Treat exact as prefix-with-no-extension. nvelox doesn't have
		// a dedicated exact matcher yet; this is a known approximation
		// we'll tighten when nvelox grows one.
		return prefix
	}
	return prefix
}

func backendName(ns, svc string, port int) string {
	return fmt.Sprintf("k8s-%s-%s-%d", ns, svc, port)
}

func listenerName(proto, ns, name, host string) string {
	// host may be empty (default match-all). DNS-1123-ish flattening
	// so the listener name is stable across reloads and grep-friendly
	// in logs.
	flat := strings.ReplaceAll(host, ".", "-")
	flat = strings.ReplaceAll(flat, "*", "_")
	if flat == "" {
		flat = "default"
	}
	return fmt.Sprintf("%s-%s-%s-%s", proto, ns, name, flat)
}
