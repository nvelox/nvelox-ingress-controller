// Translator tests — golden-output for the Ingress→nvelox-YAML
// mapping. Goal: every change that touches Render() has to face a
// diff against these expectations, so renaming a field or reordering
// a stable-sort key shows up as a test failure rather than a quiet
// production regression.
//
// Fixtures are constructed inline (typed Ingress objects) rather
// than loaded from disk — keeps the test self-contained and lets
// you Cmd-click straight from a failure to the input that produced
// it. Expected output is a raw string literal: when it deliberately
// changes, the diff in PR review is the actual YAML change.
package translator

import (
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ingress builds the smallest valid Ingress fixture inline. tlsSecrets
// optional — pass {"host": "secret-name"} to add spec.tls entries.
func ingress(ns, name string, rules []networkingv1.IngressRule, tlsSecrets map[string]string) networkingv1.Ingress {
	ing := networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       networkingv1.IngressSpec{Rules: rules},
	}
	// Group hosts by secret so we emit one tls[] entry per secret —
	// closer to how real users author Ingresses.
	bySecret := map[string][]string{}
	for host, secret := range tlsSecrets {
		bySecret[secret] = append(bySecret[secret], host)
	}
	for secret, hosts := range bySecret {
		ing.Spec.TLS = append(ing.Spec.TLS, networkingv1.IngressTLS{
			Hosts:      hosts,
			SecretName: secret,
		})
	}
	return ing
}

// rule builds a single host rule with one or more (path, service, port) tuples.
func rule(host string, paths ...pathSpec) networkingv1.IngressRule {
	prefix := networkingv1.PathTypePrefix
	r := networkingv1.IngressRule{Host: host}
	r.HTTP = &networkingv1.HTTPIngressRuleValue{}
	for _, p := range paths {
		r.HTTP.Paths = append(r.HTTP.Paths, networkingv1.HTTPIngressPath{
			Path:     p.path,
			PathType: &prefix,
			Backend: networkingv1.IngressBackend{
				Service: &networkingv1.IngressServiceBackend{
					Name: p.service,
					Port: networkingv1.ServiceBackendPort{Number: int32(p.port)},
				},
			},
		})
	}
	return r
}

type pathSpec struct {
	path    string
	service string
	port    int
}

func TestRender_EmptyCluster_NoDefaultBackend(t *testing.T) {
	// No Ingresses, no default backend → empty config. nvelox
	// include glob picks up an empty file, binds nothing, port stays
	// closed. This is the "smaller surface" posture some operators
	// prefer.
	got, err := Render(Inputs{HTTPPort: 8080, HTTPSPort: 8443})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := "{}\n"
	if string(got) != want {
		t.Errorf("mismatch\n--- want ---\n%s\n--- got ---\n%s", want, string(got))
	}
}

func TestRender_EmptyCluster_WithDefaultBackend(t *testing.T) {
	// No Ingresses, default backend enabled → catch-all 404 listener.
	// Critical regression test: this is what makes the in-pod port
	// bind on a fresh cluster.
	got, err := Render(Inputs{
		HTTPPort:           8080,
		HTTPSPort:          8443,
		DefaultBackendRoot: "/etc/nvelox/default-www",
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := `listeners:
- bind: :8080
  name: k8s-http
  protocol: http
  routes:
  - match:
      path_prefix: /
    static:
      root: /etc/nvelox/default-www
    try_files:
      fallback: =404
      files:
      - $uri
`
	if string(got) != want {
		t.Errorf("mismatch\n--- want ---\n%s\n--- got ---\n%s", want, string(got))
	}
}

func TestRender_SingleHostHTTP(t *testing.T) {
	in := Inputs{
		HTTPPort:  8080,
		HTTPSPort: 8443,
		Ingresses: []networkingv1.Ingress{
			ingress("default", "echo",
				[]networkingv1.IngressRule{
					rule("echo.example.com", pathSpec{"/", "echo", 5678}),
				}, nil),
		},
	}
	got, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := `backends:
- name: k8s-default-echo-5678
  servers:
  - echo.default.svc.cluster.local:5678
listeners:
- bind: :8080
  name: k8s-http
  protocol: http
  routes:
  - backend: k8s-default-echo-5678
    match:
      host: echo.example.com
      path_prefix: /
`
	if string(got) != want {
		t.Errorf("mismatch\n--- want ---\n%s\n--- got ---\n%s", want, string(got))
	}
}

func TestRender_DefaultBackendAppendedLast(t *testing.T) {
	// Real route + default backend → user route FIRST, catch-all
	// LAST. first-match-wins means the catch-all only fires for
	// unmatched requests, which is the whole point of having one.
	in := Inputs{
		HTTPPort:           8080,
		HTTPSPort:          8443,
		DefaultBackendRoot: "/etc/nvelox/default-www",
		Ingresses: []networkingv1.Ingress{
			ingress("default", "echo",
				[]networkingv1.IngressRule{
					rule("echo.example.com", pathSpec{"/", "echo", 5678}),
				}, nil),
		},
	}
	got, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// Both routes share the SAME listener (single :8080 bind).
	// User route must appear at index 0; catch-all at index 1.
	s := string(got)
	userIdx := strings.Index(s, "backend: k8s-default-echo-5678")
	catchIdx := strings.Index(s, "/etc/nvelox/default-www")
	if userIdx < 0 || catchIdx < 0 {
		t.Fatalf("missing route in render:\n%s", s)
	}
	if userIdx > catchIdx {
		t.Errorf("catch-all route ordered BEFORE user route — would shadow real Ingresses\n%s", s)
	}
}

func TestRender_PathFanout(t *testing.T) {
	// One host, two paths, two backends. nvelox is first-match-wins;
	// translator preserves the order from the Ingress spec, so the
	// user is responsible for ordering /api before /.
	in := Inputs{
		HTTPPort:  8080,
		HTTPSPort: 8443,
		Ingresses: []networkingv1.Ingress{
			ingress("default", "shop",
				[]networkingv1.IngressRule{
					rule("shop.example.com",
						pathSpec{"/api", "api", 80},
						pathSpec{"/", "frontend", 80},
					),
				}, nil),
		},
	}
	got, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(got)
	apiIdx := strings.Index(s, "backend: k8s-default-api-80")
	frontIdx := strings.Index(s, "backend: k8s-default-frontend-80")
	if apiIdx < 0 || frontIdx < 0 {
		t.Fatalf("missing routes:\n%s", s)
	}
	if apiIdx > frontIdx {
		t.Errorf("/api route ordered AFTER / — / would shadow /api\n%s", s)
	}
}

func TestRender_MultiHost(t *testing.T) {
	in := Inputs{
		HTTPPort:  8080,
		HTTPSPort: 8443,
		Ingresses: []networkingv1.Ingress{
			ingress("default", "sites",
				[]networkingv1.IngressRule{
					rule("blog.example.com", pathSpec{"/", "blog", 80}),
					rule("store.example.com", pathSpec{"/", "store", 80}),
				}, nil),
		},
	}
	got, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(got)
	// Both hosts share the single :8080 HTTP listener and pick by
	// route.match.host. Both backends present.
	for _, expect := range []string{
		"host: blog.example.com",
		"host: store.example.com",
		"k8s-default-blog-80",
		"k8s-default-store-80",
	} {
		if !strings.Contains(s, expect) {
			t.Errorf("missing %q in render:\n%s", expect, s)
		}
	}
}

func TestRender_HTTPSWithTLSSecret(t *testing.T) {
	in := Inputs{
		HTTPPort:   8080,
		HTTPSPort:  8443,
		TLSCertDir: "/etc/nvelox/tls",
		Ingresses: []networkingv1.Ingress{
			ingress("default", "secure",
				[]networkingv1.IngressRule{
					rule("secure.example.com", pathSpec{"/", "echo", 5678}),
				},
				map[string]string{"secure.example.com": "echo-tls"}),
		},
	}
	got, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(got)
	// HTTPS listener exists, references the materialized cert/key
	// paths the reconciler will have written. Field names MUST be
	// `cert` / `key` (not `cert_file` / `key_file`) — nvelox rejects
	// the unknown fields and refuses to bind the listener.
	for _, expect := range []string{
		"bind: :8443",
		"protocol: https",
		"server_names:",
		"- secure.example.com",
		"cert: /etc/nvelox/tls/default-echo-tls.crt",
		"key: /etc/nvelox/tls/default-echo-tls.key",
	} {
		if !strings.Contains(s, expect) {
			t.Errorf("missing %q in render:\n%s", expect, s)
		}
	}
	// HTTP listener does NOT contain this route (TLS routes go to
	// the HTTPS listener, not duplicated).
	if strings.Contains(s, "name: k8s-http") {
		// HTTP listener may exist for unrelated routes; but here
		// there are none, so it should be absent.
		t.Errorf("unexpected HTTP listener for TLS-only ingress:\n%s", s)
	}
}

// namedPortIngress is the fixture shared by the two named-port tests
// below. Backend uses port.Name="http", so the resolver map is
// required to render anything.
func namedPortIngress() networkingv1.Ingress {
	prefix := networkingv1.PathTypePrefix
	return networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "named"},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{
				Host: "named.example.com",
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Path:     "/",
							PathType: &prefix,
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: "app",
									Port: networkingv1.ServiceBackendPort{Name: "http"},
								},
							},
						}},
					},
				},
			}},
		},
	}
}

func TestRender_NamedPortDroppedWithoutResolver(t *testing.T) {
	// No ServicePorts map → translator can't turn "http" into a
	// number → route is dropped silently. Preserves the v1 contract:
	// deploys that don't wire the Services informer keep working
	// exactly as before.
	got, err := Render(Inputs{
		HTTPPort:  8080,
		HTTPSPort: 8443,
		Ingresses: []networkingv1.Ingress{namedPortIngress()},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if string(got) != "{}\n" {
		t.Errorf("expected empty render with no resolver; got:\n%s", string(got))
	}
}

func TestRender_NamedPortResolved(t *testing.T) {
	// With a populated resolver map, the named port becomes a real
	// numeric port and the route renders normally.
	got, err := Render(Inputs{
		HTTPPort:  8080,
		HTTPSPort: 8443,
		Ingresses: []networkingv1.Ingress{namedPortIngress()},
		ServicePorts: map[string]int32{
			"default/app/http": 8080,
		},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(got)
	// Backend address must carry the RESOLVED numeric port.
	if !strings.Contains(s, "app.default.svc.cluster.local:8080") {
		t.Errorf("expected resolved backend addr in:\n%s", s)
	}
	// Backend name must use the resolved port too.
	if !strings.Contains(s, "k8s-default-app-8080") {
		t.Errorf("expected backend name with resolved port in:\n%s", s)
	}
}

func TestRender_NamedPortMissingFromResolverDropped(t *testing.T) {
	// Resolver map exists but doesn't contain this specific port
	// (e.g., user typo, or Services watch hasn't caught up yet).
	// Still dropped — fail-closed beats fail-with-wrong-port.
	got, err := Render(Inputs{
		HTTPPort:  8080,
		HTTPSPort: 8443,
		Ingresses: []networkingv1.Ingress{namedPortIngress()},
		ServicePorts: map[string]int32{
			"default/app/grpc": 9000, // different port name
		},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if string(got) != "{}\n" {
		t.Errorf("expected empty render when named port not in resolver; got:\n%s", string(got))
	}
}

func TestRender_RuleWithoutHTTP_Skipped(t *testing.T) {
	// A rule with no HTTP block (e.g., TCP-only Ingress shapes that
	// other controllers extend) should be a no-op for us, not crash.
	in := Inputs{
		HTTPPort:  8080,
		HTTPSPort: 8443,
		Ingresses: []networkingv1.Ingress{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "tcp-only"},
			Spec: networkingv1.IngressSpec{
				Rules: []networkingv1.IngressRule{{Host: "tcp.example.com"}}, // no HTTP
			},
		}},
	}
	got, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if string(got) != "{}\n" {
		t.Errorf("expected empty render for HTTP-less ingress; got:\n%s", string(got))
	}
}

func TestRender_Deterministic(t *testing.T) {
	// Two Ingresses in different declaration orders MUST produce
	// identical output — otherwise the reloader's hash-gate sees
	// false-positive changes and storms nvelox with no-op SIGHUPs.
	a := ingress("ns-a", "first",
		[]networkingv1.IngressRule{rule("a.example.com", pathSpec{"/", "svc-a", 80})}, nil)
	b := ingress("ns-b", "second",
		[]networkingv1.IngressRule{rule("b.example.com", pathSpec{"/", "svc-b", 80})}, nil)

	out1, err := Render(Inputs{HTTPPort: 8080, HTTPSPort: 8443, Ingresses: []networkingv1.Ingress{a, b}})
	if err != nil {
		t.Fatalf("Render order [a,b]: %v", err)
	}
	out2, err := Render(Inputs{HTTPPort: 8080, HTTPSPort: 8443, Ingresses: []networkingv1.Ingress{b, a}})
	if err != nil {
		t.Fatalf("Render order [b,a]: %v", err)
	}
	if string(out1) != string(out2) {
		t.Errorf("non-deterministic output across input orderings\n[a,b]:\n%s\n[b,a]:\n%s", string(out1), string(out2))
	}
}

func TestRender_PortDefaults(t *testing.T) {
	// Zero values for HTTPPort / HTTPSPort fall back to 8080 / 8443.
	// Locks the defaults so production deploys depending on them
	// don't break silently if someone removes the fallback.
	in := Inputs{
		DefaultBackendRoot: "/etc/nvelox/default-www",
	}
	got, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(string(got), "bind: :8080") {
		t.Errorf("expected default :8080 bind; got:\n%s", string(got))
	}
}

func TestRender_RedirectHTTPS_TLSHost(t *testing.T) {
	// Annotation set + host has TLS → 301 redirect on HTTP listener
	// + real route still on HTTPS listener.
	i := ingress("default", "secure",
		[]networkingv1.IngressRule{
			rule("secure.example.com", pathSpec{"/", "echo", 5678}),
		},
		map[string]string{"secure.example.com": "echo-tls"})
	i.Annotations = map[string]string{"nvelox.io/redirect-https": "true"}

	got, err := Render(Inputs{
		HTTPPort:   8080,
		HTTPSPort:  8443,
		TLSCertDir: "/etc/nvelox/tls",
		Ingresses:  []networkingv1.Ingress{i},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(got)

	// HTTP listener exists (for the redirect) AND has a redirect route.
	if !strings.Contains(s, "name: k8s-http") {
		t.Errorf("expected HTTP listener (for redirect route):\n%s", s)
	}
	if !strings.Contains(s, "url: https://${host}${uri}") {
		t.Errorf("expected redirect URL template:\n%s", s)
	}
	if !strings.Contains(s, "code: 301") {
		t.Errorf("expected 301 status code:\n%s", s)
	}
	// HTTPS listener still serves the real backend.
	if !strings.Contains(s, "backend: k8s-default-echo-5678") {
		t.Errorf("expected real route on HTTPS listener:\n%s", s)
	}
}

func TestRender_RedirectHTTPS_NoTLSHost(t *testing.T) {
	// Annotation set but host has NO TLS → no redirect (can't
	// redirect to something that doesn't exist). The route still
	// serves over HTTP normally.
	i := ingress("default", "plain",
		[]networkingv1.IngressRule{
			rule("plain.example.com", pathSpec{"/", "echo", 5678}),
		}, nil) // no TLS
	i.Annotations = map[string]string{"nvelox.io/redirect-https": "true"}

	got, err := Render(Inputs{
		HTTPPort:  8080,
		HTTPSPort: 8443,
		Ingresses: []networkingv1.Ingress{i},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(got)
	if strings.Contains(s, "redirect:") || strings.Contains(s, "url: https://") {
		t.Errorf("unexpected redirect for non-TLS host:\n%s", s)
	}
	// Real route still present.
	if !strings.Contains(s, "backend: k8s-default-echo-5678") {
		t.Errorf("expected real HTTP route to survive:\n%s", s)
	}
}

func TestRender_RedirectHTTPS_DedupedPerHost(t *testing.T) {
	// Multiple paths under one TLS host → ONE redirect route, not
	// one per path. Otherwise nvelox would have duplicate match
	// rules with identical conditions, which is at best wasteful
	// and at worst a validation error.
	i := ingress("default", "multi",
		[]networkingv1.IngressRule{
			rule("multi.example.com",
				pathSpec{"/api", "api", 80},
				pathSpec{"/", "frontend", 80},
			),
		},
		map[string]string{"multi.example.com": "multi-tls"})
	i.Annotations = map[string]string{"nvelox.io/redirect-https": "true"}

	got, err := Render(Inputs{
		HTTPPort:   8080,
		HTTPSPort:  8443,
		TLSCertDir: "/etc/nvelox/tls",
		Ingresses:  []networkingv1.Ingress{i},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(got)
	// Count redirect occurrences — should be exactly 1.
	n := strings.Count(s, "url: https://${host}${uri}")
	if n != 1 {
		t.Errorf("expected exactly 1 redirect route, got %d:\n%s", n, s)
	}
}

func TestRender_RedirectHTTPS_AnnotationFalse_NoRedirect(t *testing.T) {
	// Annotation explicitly false → no redirect even when TLS is
	// present. Verifies the gate is actually checking the parsed
	// value, not just "key present".
	i := ingress("default", "secure",
		[]networkingv1.IngressRule{
			rule("secure.example.com", pathSpec{"/", "echo", 5678}),
		},
		map[string]string{"secure.example.com": "echo-tls"})
	i.Annotations = map[string]string{"nvelox.io/redirect-https": "false"}

	got, err := Render(Inputs{
		HTTPPort:   8080,
		HTTPSPort:  8443,
		TLSCertDir: "/etc/nvelox/tls",
		Ingresses:  []networkingv1.Ingress{i},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(got)
	if strings.Contains(s, "redirect:") {
		t.Errorf("annotation=false should not produce redirect:\n%s", s)
	}
}

func TestRender_RateLimit_PerSecondOnly(t *testing.T) {
	i := ingress("default", "throttled",
		[]networkingv1.IngressRule{
			rule("api.example.com", pathSpec{"/", "echo", 5678}),
		}, nil)
	i.Annotations = map[string]string{"nvelox.io/rate-limit-per-second": "50"}

	got, err := Render(Inputs{
		HTTPPort: 8080, HTTPSPort: 8443,
		Ingresses: []networkingv1.Ingress{i},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(got)
	for _, expect := range []string{
		"ip_rate_limit:",
		"requests_per_second: 50",
		"burst: 50",
	} {
		if !strings.Contains(s, expect) {
			t.Errorf("missing %q in render:\n%s", expect, s)
		}
	}
}

func TestRender_RateLimit_PerMinuteOnly(t *testing.T) {
	// 600/min → 10/sec (600/60). Burst defaults to the minute value
	// so a single client can spend its whole minute's worth at once.
	i := ingress("default", "throttled-min",
		[]networkingv1.IngressRule{
			rule("api.example.com", pathSpec{"/", "echo", 5678}),
		}, nil)
	i.Annotations = map[string]string{"nvelox.io/rate-limit-per-minute": "600"}

	got, err := Render(Inputs{
		HTTPPort: 8080, HTTPSPort: 8443,
		Ingresses: []networkingv1.Ingress{i},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(got)
	if !strings.Contains(s, "requests_per_second: 10") {
		t.Errorf("expected rps=10 from 600/min, render:\n%s", s)
	}
	if !strings.Contains(s, "burst: 600") {
		t.Errorf("expected burst=600 (per-minute value), render:\n%s", s)
	}
}

func TestRender_RateLimit_PerMinuteCeilDiv(t *testing.T) {
	// 30/min should ceil-div to 1/sec (NOT 0/sec, which would mean
	// "off"). Without the ceil, very low per-minute limits silently
	// disappear.
	i := ingress("default", "low-min",
		[]networkingv1.IngressRule{
			rule("api.example.com", pathSpec{"/", "echo", 5678}),
		}, nil)
	i.Annotations = map[string]string{"nvelox.io/rate-limit-per-minute": "30"}

	got, err := Render(Inputs{
		HTTPPort: 8080, HTTPSPort: 8443,
		Ingresses: []networkingv1.Ingress{i},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(got)
	if !strings.Contains(s, "requests_per_second: 1") {
		t.Errorf("expected rps=1 from 30/min (ceil), render:\n%s", s)
	}
}

func TestRender_RateLimit_BothSet_MoreRestrictiveWins(t *testing.T) {
	// per-second=10 + per-minute=300 → minute is stricter (5/sec).
	// Translator must pick rps=5.
	i := ingress("default", "both",
		[]networkingv1.IngressRule{
			rule("api.example.com", pathSpec{"/", "echo", 5678}),
		}, nil)
	i.Annotations = map[string]string{
		"nvelox.io/rate-limit-per-second": "10",
		"nvelox.io/rate-limit-per-minute": "300",
	}

	got, err := Render(Inputs{
		HTTPPort: 8080, HTTPSPort: 8443,
		Ingresses: []networkingv1.Ingress{i},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(got)
	if !strings.Contains(s, "requests_per_second: 5") {
		t.Errorf("expected most-restrictive rps=5 (from 300/min), render:\n%s", s)
	}
}

func TestRender_RateLimit_BothSet_PerSecondStricter(t *testing.T) {
	// per-second=10 + per-minute=1000 → per-second is stricter
	// (1000/60≈17). Translator must pick rps=10.
	i := ingress("default", "both",
		[]networkingv1.IngressRule{
			rule("api.example.com", pathSpec{"/", "echo", 5678}),
		}, nil)
	i.Annotations = map[string]string{
		"nvelox.io/rate-limit-per-second": "10",
		"nvelox.io/rate-limit-per-minute": "1000",
	}

	got, err := Render(Inputs{
		HTTPPort: 8080, HTTPSPort: 8443,
		Ingresses: []networkingv1.Ingress{i},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(got)
	if !strings.Contains(s, "requests_per_second: 10") {
		t.Errorf("expected most-restrictive rps=10 (from per-second), render:\n%s", s)
	}
}

func TestRender_RateLimit_SharedHTTPListener_MostRestrictiveWins(t *testing.T) {
	// Two HTTP-only Ingresses on the shared k8s-http listener; one
	// asks for 100/s, the other for 10/s. Listener gets one
	// ip_rate_limit block — the more restrictive (10/s) applies to
	// the whole listener. Documented behavior; per-route rate-limit
	// requires nvelox route-level rate_limit support (deferred).
	a := ingress("default", "ingress-loose",
		[]networkingv1.IngressRule{
			rule("loose.example.com", pathSpec{"/", "echo", 5678}),
		}, nil)
	a.Annotations = map[string]string{"nvelox.io/rate-limit-per-second": "100"}

	b := ingress("default", "ingress-tight",
		[]networkingv1.IngressRule{
			rule("tight.example.com", pathSpec{"/", "echo", 5678}),
		}, nil)
	b.Annotations = map[string]string{"nvelox.io/rate-limit-per-second": "10"}

	got, err := Render(Inputs{
		HTTPPort: 8080, HTTPSPort: 8443,
		Ingresses: []networkingv1.Ingress{a, b},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(got)
	if !strings.Contains(s, "requests_per_second: 10") {
		t.Errorf("expected most-restrictive rps=10 across shared listener, render:\n%s", s)
	}
	if strings.Contains(s, "requests_per_second: 100") {
		t.Errorf("looser limit should not appear; render:\n%s", s)
	}
}

func TestRender_RateLimit_NoAnnotation_NoBlock(t *testing.T) {
	// Sanity: zero annotations → no ip_rate_limit block. Listener
	// stays at default (no limit).
	i := ingress("default", "plain",
		[]networkingv1.IngressRule{
			rule("api.example.com", pathSpec{"/", "echo", 5678}),
		}, nil)
	got, err := Render(Inputs{
		HTTPPort: 8080, HTTPSPort: 8443,
		Ingresses: []networkingv1.Ingress{i},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(string(got), "ip_rate_limit") {
		t.Errorf("unexpected ip_rate_limit block without annotation:\n%s", string(got))
	}
}

func TestRender_StickyCookie_Attached(t *testing.T) {
	i := ingress("default", "sticky",
		[]networkingv1.IngressRule{
			rule("app.example.com", pathSpec{"/", "echo", 5678}),
		}, nil)
	i.Annotations = map[string]string{"nvelox.io/sticky-cookie": "NVELOX_SRV"}

	got, err := Render(Inputs{
		HTTPPort: 8080, HTTPSPort: 8443,
		Ingresses: []networkingv1.Ingress{i},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(got)
	for _, expect := range []string{
		"sticky_session:",
		"type: cookie",
		"cookie_name: NVELOX_SRV",
		"ttl: 1h",
	} {
		if !strings.Contains(s, expect) {
			t.Errorf("missing %q in render:\n%s", expect, s)
		}
	}
}

func TestRender_StickyCookie_NoAnnotation_NoBlock(t *testing.T) {
	i := ingress("default", "plain",
		[]networkingv1.IngressRule{
			rule("app.example.com", pathSpec{"/", "echo", 5678}),
		}, nil)
	got, err := Render(Inputs{
		HTTPPort: 8080, HTTPSPort: 8443,
		Ingresses: []networkingv1.Ingress{i},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(string(got), "sticky_session") {
		t.Errorf("unexpected sticky_session block:\n%s", string(got))
	}
}

func TestRender_StickyCookie_FirstWinsAcrossIngresses(t *testing.T) {
	// Two Ingresses pointing at the same backend with different
	// sticky-cookie names. Backends dedupe by name; sticky resolves
	// first-wins based on iteration order. Iteration order is
	// stable (sorted by ns/name), so "a-first" comes before
	// "b-second" → "from-a" wins.
	a := ingress("default", "a-first",
		[]networkingv1.IngressRule{
			rule("a.example.com", pathSpec{"/", "echo", 5678}),
		}, nil)
	a.Annotations = map[string]string{"nvelox.io/sticky-cookie": "from-a"}

	b := ingress("default", "b-second",
		[]networkingv1.IngressRule{
			rule("b.example.com", pathSpec{"/", "echo", 5678}),
		}, nil)
	b.Annotations = map[string]string{"nvelox.io/sticky-cookie": "from-b"}

	got, err := Render(Inputs{
		HTTPPort: 8080, HTTPSPort: 8443,
		Ingresses: []networkingv1.Ingress{b, a}, // intentionally reversed
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(got)
	if !strings.Contains(s, "cookie_name: from-a") {
		t.Errorf("expected cookie_name=from-a (stable sort puts a-first before b-second), render:\n%s", s)
	}
	if strings.Contains(s, "cookie_name: from-b") {
		t.Errorf("loser cookie name should not appear, render:\n%s", s)
	}
}

func TestRender_StickyCookie_AppliesOnlyToReferencedBackend(t *testing.T) {
	// Ingress with sticky-cookie references service "sticky-svc".
	// A second Ingress (no annotation) references "plain-svc".
	// Only the first backend should carry the sticky block.
	s1 := ingress("default", "sticky-one",
		[]networkingv1.IngressRule{
			rule("sticky.example.com", pathSpec{"/", "sticky-svc", 80}),
		}, nil)
	s1.Annotations = map[string]string{"nvelox.io/sticky-cookie": "SESS"}

	s2 := ingress("default", "plain-one",
		[]networkingv1.IngressRule{
			rule("plain.example.com", pathSpec{"/", "plain-svc", 80}),
		}, nil)

	got, err := Render(Inputs{
		HTTPPort: 8080, HTTPSPort: 8443,
		Ingresses: []networkingv1.Ingress{s1, s2},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(got)
	// Exactly one sticky_session block.
	if n := strings.Count(s, "sticky_session"); n != 1 {
		t.Errorf("expected 1 sticky_session block, got %d:\n%s", n, s)
	}
	if !strings.Contains(s, "cookie_name: SESS") {
		t.Errorf("expected cookie_name=SESS, render:\n%s", s)
	}
}

func TestRender_AllowCIDRs_HTTP(t *testing.T) {
	i := ingress("default", "internal-only",
		[]networkingv1.IngressRule{
			rule("internal.example.com", pathSpec{"/", "echo", 5678}),
		}, nil)
	i.Annotations = map[string]string{"nvelox.io/allow-cidrs": "10.0.0.0/8,192.168.0.0/16"}

	got, err := Render(Inputs{
		HTTPPort: 8080, HTTPSPort: 8443,
		Ingresses: []networkingv1.Ingress{i},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(got)
	for _, expect := range []string{
		"ip_allowlist:",
		"- 10.0.0.0/8",
		"- 192.168.0.0/16",
	} {
		if !strings.Contains(s, expect) {
			t.Errorf("missing %q in render:\n%s", expect, s)
		}
	}
}

func TestRender_DenyCIDRs_HTTP(t *testing.T) {
	i := ingress("default", "blocklist",
		[]networkingv1.IngressRule{
			rule("public.example.com", pathSpec{"/", "echo", 5678}),
		}, nil)
	i.Annotations = map[string]string{"nvelox.io/deny-cidrs": "1.2.3.0/24"}

	got, err := Render(Inputs{
		HTTPPort: 8080, HTTPSPort: 8443,
		Ingresses: []networkingv1.Ingress{i},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(got)
	for _, expect := range []string{
		"ip_denylist:",
		"- 1.2.3.0/24",
	} {
		if !strings.Contains(s, expect) {
			t.Errorf("missing %q in render:\n%s", expect, s)
		}
	}
}

func TestRender_CIDRs_UnionAcrossIngresses(t *testing.T) {
	// Two HTTP Ingresses on the shared listener each contribute
	// their own denies. UNION should produce both — denies are
	// safely additive; missing any of them would be a permission
	// regression.
	a := ingress("default", "blocklist-a",
		[]networkingv1.IngressRule{
			rule("a.example.com", pathSpec{"/", "echo", 5678}),
		}, nil)
	a.Annotations = map[string]string{"nvelox.io/deny-cidrs": "1.2.3.0/24"}

	b := ingress("default", "blocklist-b",
		[]networkingv1.IngressRule{
			rule("b.example.com", pathSpec{"/", "echo", 5678}),
		}, nil)
	b.Annotations = map[string]string{"nvelox.io/deny-cidrs": "5.6.7.0/24"}

	got, err := Render(Inputs{
		HTTPPort: 8080, HTTPSPort: 8443,
		Ingresses: []networkingv1.Ingress{a, b},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(got)
	for _, expect := range []string{
		"- 1.2.3.0/24",
		"- 5.6.7.0/24",
	} {
		if !strings.Contains(s, expect) {
			t.Errorf("expected %q in union of denies:\n%s", expect, s)
		}
	}
}

func TestRender_CIDRs_DedupAcrossIngresses(t *testing.T) {
	// Same CIDR on two Ingresses → renders ONCE. Otherwise the
	// nvelox config would have duplicate entries, which is at best
	// noisy and may upset the validator.
	a := ingress("default", "dupe-a",
		[]networkingv1.IngressRule{
			rule("a.example.com", pathSpec{"/", "echo", 5678}),
		}, nil)
	a.Annotations = map[string]string{"nvelox.io/deny-cidrs": "1.2.3.0/24"}

	b := ingress("default", "dupe-b",
		[]networkingv1.IngressRule{
			rule("b.example.com", pathSpec{"/", "echo", 5678}),
		}, nil)
	b.Annotations = map[string]string{"nvelox.io/deny-cidrs": "1.2.3.0/24"}

	got, err := Render(Inputs{
		HTTPPort: 8080, HTTPSPort: 8443,
		Ingresses: []networkingv1.Ingress{a, b},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if n := strings.Count(string(got), "- 1.2.3.0/24"); n != 1 {
		t.Errorf("expected exactly 1 occurrence of 1.2.3.0/24, got %d:\n%s", n, string(got))
	}
}

func TestRender_CIDRs_HTTPSIsolation(t *testing.T) {
	// HTTPS hosts get their own listener; an allowlist on one
	// HTTPS Ingress must NOT spill onto another HTTPS host's
	// listener.
	a := ingress("default", "secure-strict",
		[]networkingv1.IngressRule{
			rule("strict.example.com", pathSpec{"/", "echo", 5678}),
		},
		map[string]string{"strict.example.com": "strict-tls"})
	a.Annotations = map[string]string{"nvelox.io/allow-cidrs": "10.0.0.0/8"}

	b := ingress("default", "secure-open",
		[]networkingv1.IngressRule{
			rule("open.example.com", pathSpec{"/", "echo", 5678}),
		},
		map[string]string{"open.example.com": "open-tls"})

	got, err := Render(Inputs{
		HTTPPort: 8080, HTTPSPort: 8443,
		TLSCertDir: "/etc/nvelox/tls",
		Ingresses:  []networkingv1.Ingress{a, b},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(got)
	// Exactly ONE ip_allowlist block — for the strict listener only.
	if n := strings.Count(s, "ip_allowlist"); n != 1 {
		t.Errorf("expected 1 ip_allowlist (strict host only), got %d:\n%s", n, s)
	}
}

func TestRender_CIDRs_NoAnnotation_NoFilter(t *testing.T) {
	i := ingress("default", "plain",
		[]networkingv1.IngressRule{
			rule("a.example.com", pathSpec{"/", "echo", 5678}),
		}, nil)
	got, err := Render(Inputs{
		HTTPPort: 8080, HTTPSPort: 8443,
		Ingresses: []networkingv1.Ingress{i},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(got)
	if strings.Contains(s, "ip_allowlist") || strings.Contains(s, "ip_denylist") {
		t.Errorf("no annotation should produce no filter blocks:\n%s", s)
	}
}

func TestRender_StripPrefix(t *testing.T) {
	i := ingress("default", "api",
		[]networkingv1.IngressRule{
			rule("api.example.com", pathSpec{"/api/v1", "echo", 5678}),
		}, nil)
	i.Annotations = map[string]string{"nvelox.io/strip-prefix": "/api"}

	got, err := Render(Inputs{
		HTTPPort: 8080, HTTPSPort: 8443,
		Ingresses: []networkingv1.Ingress{i},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(got)
	// Route swaps path_prefix for path_regex + rewrite.
	for _, expect := range []string{
		`path_regex: ^/api/v1(.*)$`,
		`path: /v1$1`,
	} {
		if !strings.Contains(s, expect) {
			t.Errorf("missing %q in render:\n%s", expect, s)
		}
	}
	// And the old path_prefix is gone.
	if strings.Contains(s, "path_prefix: /api/v1") {
		t.Errorf("path_prefix should be replaced by path_regex when stripping:\n%s", s)
	}
}

func TestRender_StripPrefix_NoMatch_LeavesRouteAlone(t *testing.T) {
	// Strip prefix doesn't match the route's path → no rewrite,
	// no regex switch. Original path_prefix preserved.
	i := ingress("default", "frontend",
		[]networkingv1.IngressRule{
			rule("frontend.example.com", pathSpec{"/", "echo", 5678}),
		}, nil)
	i.Annotations = map[string]string{"nvelox.io/strip-prefix": "/api"}

	got, err := Render(Inputs{
		HTTPPort: 8080, HTTPSPort: 8443,
		Ingresses: []networkingv1.Ingress{i},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(got)
	if strings.Contains(s, "rewrite") || strings.Contains(s, "path_regex") {
		t.Errorf("non-matching strip-prefix should not produce rewrite:\n%s", s)
	}
	if !strings.Contains(s, "path_prefix: /") {
		t.Errorf("original path_prefix should survive:\n%s", s)
	}
}

func TestRender_StripPrefix_FullPath(t *testing.T) {
	// Strip-prefix equals the path exactly → rewrite uses just $1.
	i := ingress("default", "api",
		[]networkingv1.IngressRule{
			rule("api.example.com", pathSpec{"/api", "echo", 5678}),
		}, nil)
	i.Annotations = map[string]string{"nvelox.io/strip-prefix": "/api"}

	got, err := Render(Inputs{
		HTTPPort: 8080, HTTPSPort: 8443,
		Ingresses: []networkingv1.Ingress{i},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(got)
	if !strings.Contains(s, "path: $1") {
		t.Errorf("expected bare $1 rewrite when path == strip-prefix:\n%s", s)
	}
}

func TestRender_RequestResponseHeaders(t *testing.T) {
	i := ingress("default", "headered",
		[]networkingv1.IngressRule{
			rule("app.example.com", pathSpec{"/", "echo", 5678}),
		}, nil)
	i.Annotations = map[string]string{
		"nvelox.io/request-headers":  "X-Forwarded-By: nvelox\nX-Env: prod",
		"nvelox.io/response-headers": "Strict-Transport-Security: max-age=31536000",
	}

	got, err := Render(Inputs{
		HTTPPort: 8080, HTTPSPort: 8443,
		Ingresses: []networkingv1.Ingress{i},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(got)
	for _, expect := range []string{
		"headers:",
		"request_add:",
		"X-Forwarded-By: nvelox",
		"X-Env: prod",
		"response_add:",
		"Strict-Transport-Security: max-age=31536000",
	} {
		if !strings.Contains(s, expect) {
			t.Errorf("missing %q in render:\n%s", expect, s)
		}
	}
}

func TestRender_Headers_NoAnnotation_NoBlock(t *testing.T) {
	i := ingress("default", "plain",
		[]networkingv1.IngressRule{
			rule("app.example.com", pathSpec{"/", "echo", 5678}),
		}, nil)
	got, err := Render(Inputs{
		HTTPPort: 8080, HTTPSPort: 8443,
		Ingresses: []networkingv1.Ingress{i},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(string(got), "headers:") {
		t.Errorf("no header annotations should produce no headers block:\n%s", string(got))
	}
}

func TestRender_EndpointAddresses_OverrideServiceDNS(t *testing.T) {
	// When EndpointAddresses has entries for a backend, the
	// translator emits one server per pod IP and skips the
	// Service-DNS fallback. Proves the per-pod routing path.
	i := ingress("default", "echo",
		[]networkingv1.IngressRule{
			rule("api.example.com", pathSpec{"/", "echo", 5678}),
		}, nil)

	got, err := Render(Inputs{
		HTTPPort: 8080, HTTPSPort: 8443,
		Ingresses: []networkingv1.Ingress{i},
		EndpointAddresses: map[string][]string{
			"default/echo/5678": {"10.1.0.5:5678", "10.1.0.6:5678", "10.1.0.7:5678"},
		},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(got)
	for _, expect := range []string{
		"- 10.1.0.5:5678",
		"- 10.1.0.6:5678",
		"- 10.1.0.7:5678",
	} {
		if !strings.Contains(s, expect) {
			t.Errorf("missing %q in render:\n%s", expect, s)
		}
	}
	// Service DNS fallback must NOT appear when we have endpoints.
	if strings.Contains(s, "echo.default.svc.cluster.local") {
		t.Errorf("Service DNS fallback present when EndpointAddresses set:\n%s", s)
	}
}

func TestRender_EndpointAddresses_EmptyFallsBackToServiceDNS(t *testing.T) {
	// Empty EndpointAddresses for a backend → Service DNS fallback,
	// preserving v1 behavior. Same path fires when endpoints
	// haven't synced yet on a fresh Service.
	i := ingress("default", "echo",
		[]networkingv1.IngressRule{
			rule("api.example.com", pathSpec{"/", "echo", 5678}),
		}, nil)
	got, err := Render(Inputs{
		HTTPPort: 8080, HTTPSPort: 8443,
		Ingresses: []networkingv1.Ingress{i},
		EndpointAddresses: map[string][]string{
			"default/echo/5678": {}, // explicitly empty
		},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(string(got), "echo.default.svc.cluster.local:5678") {
		t.Errorf("expected Service DNS fallback when endpoints empty:\n%s", string(got))
	}
}

func TestRender_BackendNameStable(t *testing.T) {
	// Backend name is the join key between routes and backends — if
	// it ever changes (e.g., adding the protocol to the name), the
	// route's `backend: …` reference has to track. This test pins
	// the format so a thoughtless refactor breaks loudly.
	in := Inputs{
		HTTPPort:  8080,
		HTTPSPort: 8443,
		Ingresses: []networkingv1.Ingress{
			ingress("prod", "api",
				[]networkingv1.IngressRule{rule("api.example.com", pathSpec{"/", "api-svc", 8443})}, nil),
		},
	}
	got, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(got)
	// Format: k8s-<ns>-<svc>-<port>
	want := "k8s-prod-api-svc-8443"
	if !strings.Contains(s, "name: "+want) {
		t.Errorf("backend name format changed; expected %q in:\n%s", want, s)
	}
	if !strings.Contains(s, "backend: "+want) {
		t.Errorf("route reference doesn't match backend name; expected %q in:\n%s", want, s)
	}
}

// TestRender_TrustedProxies_EmittedOnEveryListener verifies the global
// TrustedProxies list lands on EVERY generated listener (HTTP catch-all
// + each HTTPS site). This is what makes nvelox APPEND to an upstream's
// X-Forwarded-For instead of overwriting it — the fix for client-IP
// loss when this nvelox runs behind an edge GW.
func TestRender_TrustedProxies_EmittedOnEveryListener(t *testing.T) {
	in := Inputs{
		HTTPPort:       8080,
		HTTPSPort:      8443,
		TLSCertDir:     "/etc/nvelox/tls",
		TrustedProxies: []string{"10.0.0.0/8", "192.168.0.0/16"},
		Ingresses: []networkingv1.Ingress{
			ingress("ns", "plain", []networkingv1.IngressRule{
				rule("http.example.com", pathSpec{"/", "web", 80}),
			}, nil),
			ingress("ns", "secure", []networkingv1.IngressRule{
				rule("tls.example.com", pathSpec{"/", "web", 80}),
			}, map[string]string{"tls.example.com": "tls-secret"}),
		},
	}
	got, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(got)
	// Two listeners (one HTTP, one HTTPS) → trusted_proxies appears twice,
	// each carrying both CIDRs.
	if n := strings.Count(s, "trusted_proxies:"); n != 2 {
		t.Errorf("expected trusted_proxies on 2 listeners, found %d block(s):\n%s", n, s)
	}
	if !strings.Contains(s, "10.0.0.0/8") || !strings.Contains(s, "192.168.0.0/16") {
		t.Errorf("trusted_proxies CIDRs missing from output:\n%s", s)
	}
}

// TestRender_TrustedProxies_OmittedWhenUnset guards the safe default:
// with no TrustedProxies, the field must NOT appear (omitempty), so a
// true-edge deployment keeps overwriting forged XFF rather than trusting
// nobody-knows-who.
func TestRender_TrustedProxies_OmittedWhenUnset(t *testing.T) {
	got, err := Render(Inputs{
		HTTPPort: 8080,
		HTTPSPort: 8443,
		Ingresses: []networkingv1.Ingress{
			ingress("ns", "plain", []networkingv1.IngressRule{
				rule("http.example.com", pathSpec{"/", "web", 80}),
			}, nil),
		},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(string(got), "trusted_proxies") {
		t.Errorf("trusted_proxies must be omitted when unset:\n%s", string(got))
	}
}
