// Package annotations parses nvelox.io/* annotations off an
// Ingress into a typed Spec the translator can act on.
//
// Design:
//   - One Spec struct per Ingress; the translator works with the
//     parsed struct, never with raw annotation strings. Keeps
//     translator code grep-friendly ("ann.RedirectHTTPS" beats
//     ing.Annotations["nvelox.io/redirect-https"] sprinkled
//     everywhere).
//   - Adding a new annotation = one struct field + one parse line
//   - one doc entry. Optimized for the next ten annotations,
//     not the first.
//   - Invalid values are logged + dropped + Event-emitted (TODO).
//     Never block reconcile on a parse error — the rest of the
//     Ingress should still render.
//   - Precedence: annotation > IngressClass parameters (v2) >
//     defaults. We only do "annotation > default" today.
package annotations

import (
	"log/slog"
	"net"
	"strconv"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
)

// Prefix is the annotation namespace every key shares. Kept as a
// constant so a future controller-runtime ownership scheme (e.g.,
// `nvelox.example.com/X` for a fork) is a one-line change.
const Prefix = "nvelox.io/"

// Named keys — exported so tests + samples can reference them by
// name without re-typing the prefix.
const (
	KeyRedirectHTTPS      = Prefix + "redirect-https"
	KeyRateLimitPerSecond = Prefix + "rate-limit-per-second"
	KeyRateLimitPerMinute = Prefix + "rate-limit-per-minute"
	KeyStickyCookie       = Prefix + "sticky-cookie"
	KeyAllowCIDRs         = Prefix + "allow-cidrs"
	KeyDenyCIDRs          = Prefix + "deny-cidrs"
	KeyStripPrefix        = Prefix + "strip-prefix"
	KeyRequestHeaders     = Prefix + "request-headers"
	KeyResponseHeaders    = Prefix + "response-headers"
)

// Spec is the fully-parsed annotation bundle for one Ingress. Every
// field has a zero value that means "annotation absent / default
// behavior" — so a Spec built from an Ingress with no annotations
// behaves identically to "no annotations" in the translator.
type Spec struct {
	// RedirectHTTPS, when true, makes the translator emit a
	// 301 redirect route on the HTTP listener for every host that
	// also has a TLS entry in spec.tls. The HTTPS listener still
	// serves the real backend.
	RedirectHTTPS bool

	// RateLimitPerSecond and RateLimitPerMinute are per-IP request
	// limits. 0 means "not set" (annotation absent or invalid). Both
	// may be set simultaneously — the translator picks the more
	// restrictive in per-second-equivalent terms.
	//
	// Backed by nvelox's listener-level ip_rate_limit (per-listener,
	// per-client-IP). HTTPS host = own listener = isolated limit.
	// HTTP catch-all listener = shared across all HTTP Ingresses,
	// so the most-restrictive annotation wins for the whole listener.
	RateLimitPerSecond int
	RateLimitPerMinute int

	// StickyCookie, when non-empty, names the cookie nvelox uses
	// for session affinity on every backend this Ingress references.
	// Empty = no stickiness.
	//
	// CAVEAT: full effect requires per-pod routing — see #210
	// (EndpointSlices). Today the controller emits one backend
	// entry per Service with the in-cluster DNS name, so kube-proxy
	// picks the pod and nvelox can't pin the affinity. The
	// annotation still renders correctly; it just won't actually
	// stick to a specific pod until EndpointSlices lands.
	StickyCookie string

	// AllowCIDRs / DenyCIDRs are per-listener CIDR lists mapped to
	// nvelox's ip_allowlist / ip_denylist. Nil/empty = no filter.
	// Each value parses out of a comma-separated annotation; bad
	// CIDRs are dropped with a log so a typo doesn't block the
	// rest of the Ingress from rendering.
	//
	// CAVEAT: per-listener scope (same as rate-limit). Per-HTTPS-host
	// is naturally isolated. For the shared HTTP listener: deny lists
	// UNION across contributing Ingresses (safe — additive denies).
	// Allow lists also UNION, but since "no allowlist = allow all"
	// AND "allowlist set = deny everything else", any allow-cidrs
	// annotation on one Ingress affects neighbours on the shared
	// listener. Prefer deny-cidrs for shared HTTP, or move the
	// strictly-allowlisted route to HTTPS for true isolation.
	AllowCIDRs []string
	DenyCIDRs  []string

	// StripPrefix, when non-empty, makes every route this Ingress
	// emits a regex match that strips this prefix before forwarding
	// to the backend. Common pattern: spec.host serves /api/v1 but
	// the backend Service expects to see /. Empty = no rewrite.
	StripPrefix string

	// RequestHeaders / ResponseHeaders are header injections to add
	// to every route this Ingress emits. Annotation value is a
	// multi-line string of "Name: Value" pairs, one per line. Maps
	// to nvelox's route-level headers.{request_add, response_add}.
	// Bad lines (no colon, empty name) are dropped + logged.
	RequestHeaders  map[string]string
	ResponseHeaders map[string]string
}

// Parse extracts the Spec from an Ingress's annotations. Invalid
// values are dropped with a warning log; the returned Spec contains
// only the values that parsed successfully.
func Parse(ing *networkingv1.Ingress) Spec {
	if ing == nil {
		return Spec{}
	}
	return ParseAnnotations(ing.Namespace+"/"+ing.Name, ing.Annotations)
}

// ParseAnnotations is the underlying parser. Exposed so external
// callers (e.g., the IngressClass parameters loader reading a
// ConfigMap) can produce the same Spec from the same key shape
// without going through an *Ingress. `id` is just for log
// attribution when a value is invalid.
func ParseAnnotations(id string, a map[string]string) Spec {
	s := Spec{}
	if a == nil {
		return s
	}

	if v, ok := a[KeyRedirectHTTPS]; ok {
		if b, err := strconv.ParseBool(v); err == nil {
			s.RedirectHTTPS = b
		} else {
			logBadValue(id, KeyRedirectHTTPS, v, err)
		}
	}

	s.RateLimitPerSecond = parsePositiveInt(id, KeyRateLimitPerSecond, a)
	s.RateLimitPerMinute = parsePositiveInt(id, KeyRateLimitPerMinute, a)

	if v, ok := a[KeyStickyCookie]; ok {
		// Trim only; we don't strictly validate the cookie name
		// against RFC 6265 — nvelox + the browser do that, and
		// failing the reconcile because the user typed an invalid
		// name would be worse than letting it bubble up as a
		// runtime error.
		trimmed := strings.TrimSpace(v)
		if trimmed != "" {
			s.StickyCookie = trimmed
		}
	}

	s.AllowCIDRs = parseCIDRList(id, KeyAllowCIDRs, a)
	s.DenyCIDRs = parseCIDRList(id, KeyDenyCIDRs, a)

	if v, ok := a[KeyStripPrefix]; ok {
		// Must start with "/" — Ingress paths always do, and a
		// non-slash strip-prefix would only ever fail to match.
		// Accept and trim; reject bare-non-slash quietly so it
		// doesn't silently rewrite to garbage.
		trimmed := strings.TrimSpace(v)
		if strings.HasPrefix(trimmed, "/") {
			s.StripPrefix = trimmed
		} else if trimmed != "" {
			logBadValue(id, KeyStripPrefix, v, errStripPrefixNotAbs)
		}
	}

	s.RequestHeaders = parseHeaderList(id, KeyRequestHeaders, a)
	s.ResponseHeaders = parseHeaderList(id, KeyResponseHeaders, a)

	return s
}

var errStripPrefixNotAbs = parseErr("strip-prefix must start with /")

// parseHeaderList reads a multi-line "Name: Value" annotation into a
// map. Empty lines + lines without ':' are dropped + logged.
// Returns nil if every line was invalid or the annotation is absent
// (nil = no headers block emitted by the translator).
func parseHeaderList(ingressID, key string, ann map[string]string) map[string]string {
	v, ok := ann[key]
	if !ok {
		return nil
	}
	out := map[string]string{}
	for _, line := range strings.Split(v, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		idx := strings.IndexByte(line, ':')
		if idx <= 0 {
			logBadValue(ingressID, key, line, errHeaderBadLine)
			continue
		}
		name := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		if name == "" {
			logBadValue(ingressID, key, line, errHeaderBadLine)
			continue
		}
		out[name] = val
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

var errHeaderBadLine = parseErr("expected 'Name: Value' per line")

// Merge stacks IngressClass-level defaults under per-Ingress
// annotations. The per-Ingress Spec wins for any field it sets;
// the defaults fill in everything else.
//
// Semantics per field (chosen to surprise least):
//   - RedirectHTTPS  — currently ignored from defaults. Our parser
//     can't distinguish "annotation absent" from
//     "annotation=false", so a default of true
//     can't be safely overridden by an Ingress
//     that wants false. Documented limitation;
//     lift once we track presence separately.
//   - RateLimitPer*  — non-zero per-Ingress wins; else default.
//   - StickyCookie   — non-empty per-Ingress wins; else default.
//   - StripPrefix    — non-empty per-Ingress wins; else default.
//   - AllowCIDRs/DenyCIDRs — UNION (defaults + per-Ingress, deduped
//     via the translator's existing collection map).
//   - Request/ResponseHeaders — defaults first, per-Ingress overrides
//     same-named keys. Different keys merge.
func Merge(defaults, override Spec) Spec {
	out := override

	if out.RateLimitPerSecond == 0 {
		out.RateLimitPerSecond = defaults.RateLimitPerSecond
	}
	if out.RateLimitPerMinute == 0 {
		out.RateLimitPerMinute = defaults.RateLimitPerMinute
	}
	if out.StickyCookie == "" {
		out.StickyCookie = defaults.StickyCookie
	}
	if out.StripPrefix == "" {
		out.StripPrefix = defaults.StripPrefix
	}
	out.AllowCIDRs = mergeCIDRs(defaults.AllowCIDRs, override.AllowCIDRs)
	out.DenyCIDRs = mergeCIDRs(defaults.DenyCIDRs, override.DenyCIDRs)
	out.RequestHeaders = mergeHeaders(defaults.RequestHeaders, override.RequestHeaders)
	out.ResponseHeaders = mergeHeaders(defaults.ResponseHeaders, override.ResponseHeaders)
	return out
}

func mergeCIDRs(defaults, override []string) []string {
	if len(defaults) == 0 {
		return override
	}
	if len(override) == 0 {
		return defaults
	}
	seen := make(map[string]struct{}, len(defaults)+len(override))
	out := make([]string, 0, len(defaults)+len(override))
	for _, c := range append(defaults, override...) {
		if _, ok := seen[c]; ok {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	return out
}

func mergeHeaders(defaults, override map[string]string) map[string]string {
	if len(defaults) == 0 {
		return override
	}
	if len(override) == 0 {
		return defaults
	}
	out := make(map[string]string, len(defaults)+len(override))
	for k, v := range defaults {
		out[k] = v
	}
	for k, v := range override {
		out[k] = v // override wins on same key
	}
	return out
}

// parseCIDRList reads a comma-separated CIDR annotation and returns
// the validated CIDRs. Bad entries are dropped + logged so a typo in
// one CIDR doesn't poison the rest of the list (and doesn't poison
// the whole Ingress's reconcile). Returns nil when the annotation is
// absent OR every entry was invalid — nil = "no filter applied".
func parseCIDRList(ingressID, key string, ann map[string]string) []string {
	v, ok := ann[key]
	if !ok {
		return nil
	}
	var out []string
	for _, raw := range strings.Split(v, ",") {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		if _, _, err := net.ParseCIDR(entry); err != nil {
			logBadValue(ingressID, key, entry, err)
			continue
		}
		out = append(out, entry)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parsePositiveInt reads an annotation, returns 0 when absent /
// invalid / non-positive. Zero means "annotation off" everywhere in
// the codebase, so negative or zero values from the user collapse
// to "no limit" with a log so the typo is visible.
func parsePositiveInt(ingressID, key string, ann map[string]string) int {
	v, ok := ann[key]
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		logBadValue(ingressID, key, v, err)
		return 0
	}
	if n <= 0 {
		logBadValue(ingressID, key, v, errInvalidPositive)
		return 0
	}
	return n
}

var errInvalidPositive = parseErr("expected positive integer")

type parseErr string

func (e parseErr) Error() string { return string(e) }

func logBadValue(ingressID, key, value string, err error) {
	slog.Warn("ignoring invalid annotation value",
		"ingress", ingressID,
		"annotation", key,
		"value", value,
		"err", err.Error(),
	)
}
