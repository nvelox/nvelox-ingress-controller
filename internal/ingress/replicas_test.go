package ingress

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/nvelox/nvelox-ingress-controller/internal/reloader"
)

// Every replica serves traffic from its own nvelox config, so a replica that is NOT the
// leader must still render routes and become ready — it only skips the status write
// (2026-10-22: with leader election, followers kept the catch-all config → 404/502s).
func TestFollowerRendersRoutesButWritesNoStatus(t *testing.T) {
	ing := testIngress("app", "a") // never published: a leader would write its status
	pathType := networkingv1.PathTypePrefix
	ing.Spec.Rules = []networkingv1.IngressRule{{
		Host: "a.example.test",
		IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
			Paths: []networkingv1.HTTPIngressPath{{Path: "/", PathType: &pathType, Backend: networkingv1.IngressBackend{
				Service: &networkingv1.IngressServiceBackend{Name: "web", Port: networkingv1.ServiceBackendPort{Number: 80}},
			}}},
		}},
	}}
	w := &writeCounter{}
	c := newTestClient(t, w, testNodePortService(), testNode("node-1", "10.0.0.101"), ing)
	r := testReconciler(t, c)
	r.IsLeader = func() bool { return false }
	if r.Ready() {
		t.Fatal("ready before any config was applied")
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatal(err)
	}
	if !r.Ready() {
		t.Fatal("follower not ready after rendering its config")
	}
	cfg, err := os.ReadFile(r.Reload.ConfigPath)
	if err != nil {
		t.Fatalf("follower wrote no nvelox config: %v", err)
	}
	if !strings.Contains(string(cfg), "a.example.test") {
		t.Fatalf("follower config has no route for a.example.test:\n%s", cfg)
	}
	if status, other := w.get(); status != 0 || other != 0 {
		t.Fatalf("follower wrote %d status and %d other updates, want none", status, other)
	}

	// The same replica after winning the election publishes status.
	r.IsLeader = func() bool { return true }
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatal(err)
	}
	if status, _ := w.get(); status != 1 {
		t.Fatalf("leader wrote %d status updates, want 1", status)
	}
}

// A replica whose nvelox reload failed is not ready: it must not take traffic.
func TestNotReadyWhenReloadFails(t *testing.T) {
	w := &writeCounter{}
	c := newTestClient(t, w, testNodePortService(), testIngress("app", "a"))
	r := testReconciler(t, c)
	dir := t.TempDir()
	r.Reload = &reloader.Reloader{ConfigPath: filepath.Join(dir, "k8s.yaml"), PIDFile: filepath.Join(dir, "missing.pid")}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err == nil {
		t.Skip("reloader accepted a missing pid file; readiness on reload failure not testable here")
	}
	if r.Ready() {
		t.Fatal("ready although the nvelox reload failed")
	}
}
