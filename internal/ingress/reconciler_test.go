package ingress

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/nvelox/nvelox-ingress-controller/internal/reloader"
)

const (
	testClass   = "nvelox"
	testSvcNS   = "nvelox-ingress"
	testSvcName = "nvelox-ingress-controller"
)

// writeCounter counts every write the reconciler sends to the apiserver.
type writeCounter struct {
	mu     sync.Mutex
	status int // status subresource patches/updates
	other  int // any other create/update/patch/delete
}

func (w *writeCounter) get() (status, other int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status, w.other
}

// newTestClient builds a fake client that behaves like the informer-cache
// client in production in the one way that matters here: every Node List
// returns the Nodes in a different order (the cache iterates a Go map). The
// rotation is deterministic so a test that depends on it never flakes.
func newTestClient(t *testing.T, w *writeCounter, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	var nodeLists int
	countOther := func() { w.mu.Lock(); w.other++; w.mu.Unlock() }
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&networkingv1.Ingress{}).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if err := c.List(ctx, list, opts...); err != nil {
					return err
				}
				nodes, ok := list.(*corev1.NodeList)
				if !ok || len(nodes.Items) == 0 {
					return nil
				}
				slices.SortFunc(nodes.Items, func(a, b corev1.Node) int { return strings.Compare(a.Name, b.Name) })
				nodeLists++
				k := nodeLists % len(nodes.Items)
				nodes.Items = append(nodes.Items[k:], nodes.Items[:k]...)
				return nil
			},
			SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				w.mu.Lock()
				w.status++
				w.mu.Unlock()
				return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				w.mu.Lock()
				w.status++
				w.mu.Unlock()
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				countOther()
				return c.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				countOther()
				return c.Update(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				countOther()
				return c.Patch(ctx, obj, patch, opts...)
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				countOther()
				return c.Delete(ctx, obj, opts...)
			},
		}).
		Build()
}

// testReloader returns a Reloader that succeeds without a real nvelox.
// Reconcile renders + reloads BEFORE it publishes status, so the reload
// has to work for the status path to run at all. The pid file points at
// this test process and SIGHUP is captured for the test's lifetime, so the
// reload signal is harmless.
func testReloader(t *testing.T) *reloader.Reloader {
	t.Helper()
	sig := make(chan os.Signal, 16)
	signal.Notify(sig, syscall.SIGHUP)
	t.Cleanup(func() { signal.Stop(sig) })
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "nvelox.pid")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	return &reloader.Reloader{ConfigPath: filepath.Join(dir, "k8s.yaml"), PIDFile: pidFile}
}

func testNode(name, ip string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeHostName, Address: name},
			{Type: corev1.NodeInternalIP, Address: ip},
		}},
	}
}

func testIngress(ns, name string, lbIPs ...string) *networkingv1.Ingress {
	class := testClass
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       networkingv1.IngressSpec{IngressClassName: &class},
	}
	for _, ip := range lbIPs {
		ing.Status.LoadBalancer.Ingress = append(ing.Status.LoadBalancer.Ingress,
			networkingv1.IngressLoadBalancerIngress{IP: ip})
	}
	return ing
}

func testNodePortService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: testSvcNS, Name: testSvcName},
		Spec: corev1.ServiceSpec{
			Type:  corev1.ServiceTypeNodePort,
			Ports: []corev1.ServicePort{{Name: "http", Port: 80}},
		},
	}
}

func testReconciler(t *testing.T, c client.Client) *Reconciler {
	t.Helper()
	return &Reconciler{
		Client:         c,
		IngressClass:   testClass,
		HTTPPort:       8080,
		HTTPSPort:      8443,
		Reload:         testReloader(t),
		PublishService: testSvcNS + "/" + testSvcName,
	}
}

func lbIPs(t *testing.T, c client.Client, ns, name string) []string {
	t.Helper()
	var ing networkingv1.Ingress
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &ing); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ing.Status.LoadBalancer.Ingress {
		out = append(out, e.IP)
	}
	return out
}

// TestReconcileUnchangedStatusIssuesNoWrites is the regression test for the
// status PATCH loop seen in production (~6 PATCH/s for 10 Ingresses): the
// Node List order changed on every reconcile, the order-sensitive status
// comparison treated each new order as a change, and every reconcile patched
// every Ingress. With the address set unchanged, no write may be sent.
func TestReconcileUnchangedStatusIssuesNoWrites(t *testing.T) {
	w := &writeCounter{}
	c := newTestClient(t, w,
		testNodePortService(),
		testNode("node-1", "10.0.0.101"),
		testNode("node-2", "10.0.0.102"),
		testNode("node-3", "10.0.0.103"),
		// Already-published status, in sorted order and in an older
		// writer's rotated order: both hold the same address set.
		testIngress("app", "sorted", "10.0.0.101", "10.0.0.102", "10.0.0.103"),
		testIngress("app", "rotated", "10.0.0.103", "10.0.0.101", "10.0.0.102"),
	)
	r := testReconciler(t, c)

	// Several reconciles, each seeing the Nodes in a different order.
	for i := 0; i < 6; i++ {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	if status, other := w.get(); status != 0 || other != 0 {
		t.Fatalf("unchanged status: got %d status writes and %d other writes, want 0 and 0", status, other)
	}
}

// TestReconcileChangedAddressesWritesOnceSorted checks the other direction:
// a real address change is published exactly once per Ingress, in sorted
// order, and later reconciles with a different Node order send nothing.
func TestReconcileChangedAddressesWritesOnceSorted(t *testing.T) {
	notOurs := testIngress("other", "not-ours")
	foreignClass := "other-class"
	notOurs.Spec.IngressClassName = &foreignClass

	w := &writeCounter{}
	c := newTestClient(t, w,
		testNodePortService(),
		testNode("node-1", "10.0.0.101"),
		testNode("node-2", "10.0.0.102"),
		testNode("node-3", "10.0.0.103"),
		testIngress("app", "a", "10.0.0.101", "10.0.0.102"), // node-3 is new
		testIngress("app", "b"),                             // never published
		notOurs,                                             // another class: never touched
	)
	r := testReconciler(t, c)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatal(err)
	}
	if status, _ := w.get(); status != 2 {
		t.Fatalf("first reconcile: got %d status writes, want 2 (one per owned Ingress)", status)
	}
	want := []string{"10.0.0.101", "10.0.0.102", "10.0.0.103"}
	for _, name := range []string{"a", "b"} {
		if got := lbIPs(t, c, "app", name); !slices.Equal(got, want) {
			t.Errorf("app/%s status = %v, want %v", name, got, want)
		}
	}
	if got := lbIPs(t, c, "other", "not-ours"); len(got) != 0 {
		t.Errorf("foreign-class Ingress got status %v, want none", got)
	}

	for i := 0; i < 5; i++ {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	if status, other := w.get(); status != 2 || other != 0 {
		t.Fatalf("after convergence: got %d status and %d other writes in total, want 2 and 0", status, other)
	}
}

func TestIngressStatusEqualIgnoresOrder(t *testing.T) {
	lb := func(entries ...string) []networkingv1.IngressLoadBalancerIngress {
		var out []networkingv1.IngressLoadBalancerIngress
		for _, e := range entries {
			ip, host, _ := strings.Cut(e, "|")
			out = append(out, networkingv1.IngressLoadBalancerIngress{IP: ip, Hostname: host})
		}
		return out
	}
	cases := []struct {
		name string
		a, b []networkingv1.IngressLoadBalancerIngress
		want bool
	}{
		{"both empty", nil, nil, true},
		{"same order", lb("1.1.1.1", "2.2.2.2"), lb("1.1.1.1", "2.2.2.2"), true},
		{"rotated", lb("1.1.1.1", "2.2.2.2", "3.3.3.3"), lb("3.3.3.3", "1.1.1.1", "2.2.2.2"), true},
		{"hostnames reordered", lb("|a.example", "|b.example"), lb("|b.example", "|a.example"), true},
		{"different length", lb("1.1.1.1"), lb("1.1.1.1", "2.2.2.2"), false},
		{"different IP", lb("1.1.1.1", "2.2.2.2"), lb("1.1.1.1", "2.2.2.3"), false},
		{"different hostname", lb("1.1.1.1|a.example"), lb("1.1.1.1|b.example"), false},
		{"duplicate vs distinct", lb("1.1.1.1", "1.1.1.1"), lb("1.1.1.1", "2.2.2.2"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, b := slices.Clone(tc.a), slices.Clone(tc.b)
			if got := ingressStatusEqual(tc.a, tc.b); got != tc.want {
				t.Fatalf("ingressStatusEqual = %v, want %v", got, tc.want)
			}
			if !reflect.DeepEqual(a, tc.a) || !reflect.DeepEqual(b, tc.b) {
				t.Fatal("ingressStatusEqual reordered its inputs")
			}
		})
	}
}

func TestIngressEventFilter(t *testing.T) {
	p := IngressEventFilter()
	base := testIngress("app", "a")
	base.Generation = 3
	base.Annotations = map[string]string{"nvelox.io/rate-limit": "10"}

	statusOnly := base.DeepCopy()
	statusOnly.ResourceVersion = "2"
	statusOnly.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{IP: "10.0.0.101"}}

	specChanged := base.DeepCopy()
	specChanged.Generation = 4

	annotationChanged := base.DeepCopy()
	annotationChanged.Annotations = map[string]string{"nvelox.io/rate-limit": "20"}

	if p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: statusOnly}) {
		t.Error("status-only update passed the filter; it would re-trigger reconcile")
	}
	if !p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: specChanged}) {
		t.Error("spec (generation) change was filtered out")
	}
	if !p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: annotationChanged}) {
		t.Error("annotation change was filtered out")
	}
	if !p.Create(event.CreateEvent{Object: base}) {
		t.Error("create was filtered out")
	}
	if !p.Delete(event.DeleteEvent{Object: base}) {
		t.Error("delete was filtered out")
	}
}
