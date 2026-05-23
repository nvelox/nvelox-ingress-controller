package annotations

import (
	"reflect"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func ing(ann map[string]string) *networkingv1.Ingress {
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   "default",
			Name:        "test",
			Annotations: ann,
		},
	}
}

func TestParse_Empty(t *testing.T) {
	// Nil ingress + nil annotations + empty annotations all produce
	// the zero Spec. Zero Spec must be safe to pass to the translator
	// — that's the "annotation absent = default behavior" contract.
	for _, name := range []string{"nil-ingress", "nil-annotations", "empty-annotations"} {
		t.Run(name, func(t *testing.T) {
			var s Spec
			switch name {
			case "nil-ingress":
				s = Parse(nil)
			case "nil-annotations":
				s = Parse(&networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "x"}})
			case "empty-annotations":
				s = Parse(ing(map[string]string{}))
			}
			// Spec now contains slices, so direct == is illegal.
			// reflect.DeepEqual is what we actually want here: every
			// field at its zero value, including nil slices.
			if !reflect.DeepEqual(s, Spec{}) {
				t.Errorf("expected zero Spec, got %+v", s)
			}
		})
	}
}

func TestParse_RedirectHTTPS_True(t *testing.T) {
	for _, v := range []string{"true", "TRUE", "1", "t", "T"} {
		t.Run(v, func(t *testing.T) {
			s := Parse(ing(map[string]string{KeyRedirectHTTPS: v}))
			if !s.RedirectHTTPS {
				t.Errorf("expected RedirectHTTPS=true for %q, got false", v)
			}
		})
	}
}

func TestParse_RedirectHTTPS_False(t *testing.T) {
	for _, v := range []string{"false", "FALSE", "0", "f", "F"} {
		t.Run(v, func(t *testing.T) {
			s := Parse(ing(map[string]string{KeyRedirectHTTPS: v}))
			if s.RedirectHTTPS {
				t.Errorf("expected RedirectHTTPS=false for %q, got true", v)
			}
		})
	}
}

func TestParse_RedirectHTTPS_Invalid(t *testing.T) {
	// Garbage value must not panic, must not set the field, must be
	// logged (we can't easily assert the log here; covered by the
	// log handler in main + manual review). Reconcile must keep
	// processing — the rest of the Ingress should still render.
	s := Parse(ing(map[string]string{KeyRedirectHTTPS: "yes-please"}))
	if s.RedirectHTTPS {
		t.Errorf("invalid value should leave field at zero, got RedirectHTTPS=true")
	}
}

func TestParse_RateLimitPerSecond(t *testing.T) {
	cases := []struct {
		val  string
		want int
	}{
		{"100", 100},
		{"1", 1},
		{"0", 0},     // non-positive → off
		{"-5", 0},    // negative → off
		{"abc", 0},   // garbage → off
		{"", 0},      // empty → off (treated like absent)
	}
	for _, c := range cases {
		t.Run(c.val, func(t *testing.T) {
			s := Parse(ing(map[string]string{KeyRateLimitPerSecond: c.val}))
			if s.RateLimitPerSecond != c.want {
				t.Errorf("RateLimitPerSecond=%d, want %d", s.RateLimitPerSecond, c.want)
			}
		})
	}
}

func TestParse_RateLimitPerMinute(t *testing.T) {
	s := Parse(ing(map[string]string{KeyRateLimitPerMinute: "600"}))
	if s.RateLimitPerMinute != 600 {
		t.Errorf("RateLimitPerMinute=%d, want 600", s.RateLimitPerMinute)
	}
}

func TestParse_RateLimit_BothSet(t *testing.T) {
	// Both annotations parse independently — collision resolution
	// happens in the translator, not the parser.
	s := Parse(ing(map[string]string{
		KeyRateLimitPerSecond: "10",
		KeyRateLimitPerMinute: "300",
	}))
	if s.RateLimitPerSecond != 10 {
		t.Errorf("RateLimitPerSecond=%d, want 10", s.RateLimitPerSecond)
	}
	if s.RateLimitPerMinute != 300 {
		t.Errorf("RateLimitPerMinute=%d, want 300", s.RateLimitPerMinute)
	}
}

func TestParse_StickyCookie(t *testing.T) {
	cases := []struct {
		val  string
		want string
	}{
		{"NVELOX_SRV", "NVELOX_SRV"},
		{"  trimmed  ", "trimmed"},        // whitespace trimmed
		{"", ""},                          // empty → off
		{"   ", ""},                       // whitespace-only → off
	}
	for _, c := range cases {
		t.Run(c.val, func(t *testing.T) {
			s := Parse(ing(map[string]string{KeyStickyCookie: c.val}))
			if s.StickyCookie != c.want {
				t.Errorf("StickyCookie=%q, want %q", s.StickyCookie, c.want)
			}
		})
	}
}

func TestParse_StickyCookie_Absent(t *testing.T) {
	s := Parse(ing(map[string]string{}))
	if s.StickyCookie != "" {
		t.Errorf("absent annotation should leave StickyCookie empty, got %q", s.StickyCookie)
	}
}

func TestParse_AllowCIDRs(t *testing.T) {
	cases := []struct {
		val  string
		want []string
	}{
		{"10.0.0.0/8", []string{"10.0.0.0/8"}},
		{"10.0.0.0/8,192.168.0.0/16", []string{"10.0.0.0/8", "192.168.0.0/16"}},
		{"  10.0.0.0/8 , 192.168.0.0/16  ", []string{"10.0.0.0/8", "192.168.0.0/16"}}, // whitespace
		{"10.0.0.0/8,invalid-cidr,192.168.0.0/16", []string{"10.0.0.0/8", "192.168.0.0/16"}}, // bad entry dropped
		{",,,", nil},   // all empty
		{"", nil},      // empty value
		{"junk", nil},  // entire value invalid
	}
	for _, c := range cases {
		t.Run(c.val, func(t *testing.T) {
			s := Parse(ing(map[string]string{KeyAllowCIDRs: c.val}))
			if !reflect.DeepEqual(s.AllowCIDRs, c.want) {
				t.Errorf("AllowCIDRs=%v, want %v", s.AllowCIDRs, c.want)
			}
		})
	}
}

func TestParse_DenyCIDRs(t *testing.T) {
	s := Parse(ing(map[string]string{KeyDenyCIDRs: "172.16.0.0/12,fd00::/8"}))
	want := []string{"172.16.0.0/12", "fd00::/8"}
	if !reflect.DeepEqual(s.DenyCIDRs, want) {
		t.Errorf("DenyCIDRs=%v, want %v", s.DenyCIDRs, want)
	}
}

func TestParse_StripPrefix(t *testing.T) {
	cases := []struct {
		val  string
		want string
	}{
		{"/api", "/api"},
		{"  /api  ", "/api"},  // trimmed
		{"/api/v1", "/api/v1"},
		{"", ""},               // empty → off
		{"api-no-slash", ""},   // missing leading slash → rejected
	}
	for _, c := range cases {
		t.Run(c.val, func(t *testing.T) {
			s := Parse(ing(map[string]string{KeyStripPrefix: c.val}))
			if s.StripPrefix != c.want {
				t.Errorf("StripPrefix=%q, want %q", s.StripPrefix, c.want)
			}
		})
	}
}

func TestParse_RequestHeaders(t *testing.T) {
	v := "X-Forwarded-By: nvelox\nX-Env: prod\nbad-line-no-colon\n: empty-name\n"
	s := Parse(ing(map[string]string{KeyRequestHeaders: v}))
	want := map[string]string{
		"X-Forwarded-By": "nvelox",
		"X-Env":          "prod",
	}
	if !reflect.DeepEqual(s.RequestHeaders, want) {
		t.Errorf("RequestHeaders=%v, want %v", s.RequestHeaders, want)
	}
}

func TestParse_ResponseHeaders_Empty(t *testing.T) {
	// Only invalid lines → nil map (translator skips the block).
	s := Parse(ing(map[string]string{KeyResponseHeaders: "garbage\nmore garbage"}))
	if s.ResponseHeaders != nil {
		t.Errorf("expected nil ResponseHeaders for all-invalid input, got %v", s.ResponseHeaders)
	}
}

func TestMerge_DefaultsFillWhenOverrideAbsent(t *testing.T) {
	defaults := Spec{
		RateLimitPerSecond: 100,
		StickyCookie:       "DEFAULT_SRV",
		AllowCIDRs:         []string{"10.0.0.0/8"},
		RequestHeaders:     map[string]string{"X-Default": "default"},
	}
	override := Spec{} // empty
	got := Merge(defaults, override)
	if got.RateLimitPerSecond != 100 {
		t.Errorf("rps fall-through failed: got %d", got.RateLimitPerSecond)
	}
	if got.StickyCookie != "DEFAULT_SRV" {
		t.Errorf("sticky fall-through failed: got %q", got.StickyCookie)
	}
	if !reflect.DeepEqual(got.AllowCIDRs, []string{"10.0.0.0/8"}) {
		t.Errorf("allow CIDRs fall-through failed: got %v", got.AllowCIDRs)
	}
	if got.RequestHeaders["X-Default"] != "default" {
		t.Errorf("header fall-through failed: got %v", got.RequestHeaders)
	}
}

func TestMerge_OverrideWinsOnScalars(t *testing.T) {
	defaults := Spec{RateLimitPerSecond: 100, StickyCookie: "DEFAULT", StripPrefix: "/d"}
	override := Spec{RateLimitPerSecond: 5, StickyCookie: "OVERRIDE", StripPrefix: "/o"}
	got := Merge(defaults, override)
	if got.RateLimitPerSecond != 5 {
		t.Errorf("override rps lost: %d", got.RateLimitPerSecond)
	}
	if got.StickyCookie != "OVERRIDE" {
		t.Errorf("override sticky lost: %q", got.StickyCookie)
	}
	if got.StripPrefix != "/o" {
		t.Errorf("override strip lost: %q", got.StripPrefix)
	}
}

func TestMerge_CIDRsUnion(t *testing.T) {
	got := Merge(
		Spec{DenyCIDRs: []string{"1.2.3.0/24", "5.6.7.0/24"}},
		Spec{DenyCIDRs: []string{"5.6.7.0/24", "9.9.9.0/24"}},
	)
	want := []string{"1.2.3.0/24", "5.6.7.0/24", "9.9.9.0/24"}
	if !reflect.DeepEqual(got.DenyCIDRs, want) {
		t.Errorf("union+dedup failed: got %v want %v", got.DenyCIDRs, want)
	}
}

func TestMerge_HeadersOverridePerKey(t *testing.T) {
	got := Merge(
		Spec{RequestHeaders: map[string]string{"X-Foo": "default", "X-Bar": "bar"}},
		Spec{RequestHeaders: map[string]string{"X-Foo": "override"}},
	)
	if got.RequestHeaders["X-Foo"] != "override" {
		t.Errorf("X-Foo should be overridden: %v", got.RequestHeaders)
	}
	if got.RequestHeaders["X-Bar"] != "bar" {
		t.Errorf("X-Bar should fall through from defaults: %v", got.RequestHeaders)
	}
}

func TestParse_CIDRs_BothSet(t *testing.T) {
	// Allow + deny parse independently. nvelox applies both filters
	// (allowlist first, then denylist) — translator just hands the
	// lists off; semantic stacking is nvelox's job.
	s := Parse(ing(map[string]string{
		KeyAllowCIDRs: "10.0.0.0/8",
		KeyDenyCIDRs:  "10.99.0.0/16",
	}))
	if !reflect.DeepEqual(s.AllowCIDRs, []string{"10.0.0.0/8"}) {
		t.Errorf("AllowCIDRs=%v", s.AllowCIDRs)
	}
	if !reflect.DeepEqual(s.DenyCIDRs, []string{"10.99.0.0/16"}) {
		t.Errorf("DenyCIDRs=%v", s.DenyCIDRs)
	}
}
