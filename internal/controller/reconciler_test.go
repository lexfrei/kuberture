package controller

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	applycorev1 "k8s.io/client-go/applyconfigurations/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/lexfrei/kuberture/internal/config"
	"github.com/lexfrei/kuberture/internal/resolver"
)

const (
	testNamespace    = "default"
	testSvcNamespace = "test-ns"
	testAddr1        = "10.0.0.1"
	testAddrPair13   = "10.0.0.1,10.0.0.3"
	testHostExample  = "example.com"
	testSvcA         = "svc-a"
	testSvcB         = "svc-b"
	testNodeExtAddr  = "203.0.113.10"
	testSvcDefault   = "kuberture-test"
)

// appliedService records what was passed to the intercepted Apply call.
type appliedService struct {
	name        string
	namespace   string
	annotations map[string]string
	clusterIP   string
	serviceType corev1.ServiceType
}

func boolPtr(val bool) *bool {
	return &val
}

func strPtr(val string) *string {
	return &val
}

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()

	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add corev1 to scheme: %v", err)
	}

	if err := discoveryv1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add discoveryv1 to scheme: %v", err)
	}

	return scheme
}

func newTestConfig(outputs []config.OutputConfig) *atomic.Pointer[config.Config] {
	cfg := &config.Config{
		Source: config.SourceConfig{
			Namespace:   testNamespace,
			ServiceName: "kubernetes",
		},
		Outputs: outputs,
	}

	ptr := &atomic.Pointer[config.Config]{}
	ptr.Store(cfg)

	return ptr
}

func makeEndpointSlice(
	name string,
	addrType discoveryv1.AddressType,
	endpoints []discoveryv1.Endpoint,
) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
			Labels: map[string]string{
				discoveryv1.LabelServiceName: "kubernetes",
			},
		},
		AddressType: addrType,
		Endpoints:   endpoints,
	}
}

func makeNode(name string, addresses []corev1.NodeAddress) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     corev1.NodeStatus{Addresses: addresses},
	}
}

func defaultOutput() config.OutputConfig {
	return config.OutputConfig{
		Name:             "test-output",
		Hostnames:        []string{testHostExample},
		AnnotationPrefix: "external-dns.alpha.kubernetes.io/",
		ServiceName:      testSvcDefault,
		RecordTTL:        60,
		AddressSource:    config.AddressSourceEndpointSlice,
		AddressType:      config.AddressTypeIPv4,
	}
}

// buildTestReconciler creates a Reconciler with a fake client backed by the
// given objects and an interceptor that captures Apply calls.
func buildTestReconciler(
	t *testing.T,
	initObjs []client.Object,
	cfgPtr *atomic.Pointer[config.Config],
	applied *[]appliedService,
	applyErr error,
) *Reconciler {
	t.Helper()

	scheme := newTestScheme(t)

	applyFn := func(
		_ context.Context,
		_ client.WithWatch,
		obj runtime.ApplyConfiguration,
		_ ...client.ApplyOption,
	) error {
		if applyErr != nil {
			return applyErr
		}

		svc, ok := obj.(*applycorev1.ServiceApplyConfiguration)
		if !ok {
			t.Fatalf("Apply called with non-Service object: %T", obj)
		}

		rec := appliedService{}

		if svc.Name != nil {
			rec.name = *svc.Name
		}

		if svc.Namespace != nil {
			rec.namespace = *svc.Namespace
		}

		if svc.Annotations != nil {
			rec.annotations = svc.Annotations
		}

		if svc.Spec != nil {
			if svc.Spec.Type != nil {
				rec.serviceType = *svc.Spec.Type
			}

			if svc.Spec.ClusterIP != nil {
				rec.clusterIP = *svc.Spec.ClusterIP
			}
		}

		*applied = append(*applied, rec)

		return nil
	}

	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(initObjs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: applyFn,
		}).
		Build()

	res := resolver.NewResolver(cli, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	log := slog.Default()

	return NewReconciler(cli, res, cfgPtr, log, "test-instance", testSvcNamespace, nil, nil)
}

func TestReconcile(t *testing.T) {
	tests := []struct {
		name         string
		objects      []client.Object
		outputs      []config.OutputConfig
		applyErr     error
		wantApplied  int
		wantErr      bool
		checkApplied func(t *testing.T, applied []appliedService)
	}{
		{
			name: "single EndpointSlice creates Service with correct annotations",
			objects: []client.Object{
				makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
					{
						Addresses:  []string{testAddr1},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
					},
				}),
			},
			outputs:     []config.OutputConfig{defaultOutput()},
			wantApplied: 1,
			checkApplied: func(t *testing.T, applied []appliedService) {
				t.Helper()
				svc := applied[0]

				if svc.name != testSvcDefault {
					t.Errorf("service name = %q, want %q", svc.name, testSvcDefault)
				}

				if svc.namespace != testSvcNamespace {
					t.Errorf("service namespace = %q, want %q", svc.namespace, testSvcNamespace)
				}

				if svc.serviceType != corev1.ServiceTypeClusterIP {
					t.Errorf("service type = %v, want ClusterIP", svc.serviceType)
				}

				if svc.clusterIP != string(corev1.ClusterIPNone) {
					t.Errorf("clusterIP = %q, want %q", svc.clusterIP, corev1.ClusterIPNone)
				}

				wantHostname := testHostExample
				gotHostname := svc.annotations["external-dns.alpha.kubernetes.io/hostname"]
				if gotHostname != wantHostname {
					t.Errorf("hostname annotation = %q, want %q", gotHostname, wantHostname)
				}

				wantTarget := testAddr1
				gotTarget := svc.annotations["external-dns.alpha.kubernetes.io/target"]
				if gotTarget != wantTarget {
					t.Errorf("target annotation = %q, want %q", gotTarget, wantTarget)
				}

				wantTTL := "60"
				gotTTL := svc.annotations["external-dns.alpha.kubernetes.io/ttl"]
				if gotTTL != wantTTL {
					t.Errorf("ttl annotation = %q, want %q", gotTTL, wantTTL)
				}
			},
		},
		{
			name: "multiple EndpointSlices aggregate and sort addresses",
			objects: []client.Object{
				makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.0.0.3"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
					},
				}),
				makeEndpointSlice("eps-2", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
					{
						Addresses:  []string{testAddr1},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
					},
				}),
			},
			outputs:     []config.OutputConfig{defaultOutput()},
			wantApplied: 1,
			checkApplied: func(t *testing.T, applied []appliedService) {
				t.Helper()
				target := applied[0].annotations["external-dns.alpha.kubernetes.io/target"]
				want := testAddrPair13
				if target != want {
					t.Errorf("target = %q, want %q", target, want)
				}
			},
		},
		{
			name: "mixed ready and not-ready endpoints uses only ready ones",
			objects: []client.Object{
				makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
					{
						Addresses:  []string{testAddr1},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
					},
					{
						Addresses:  []string{"10.0.0.2"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(false)},
					},
					{
						Addresses:  []string{"10.0.0.3"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
					},
				}),
			},
			outputs:     []config.OutputConfig{defaultOutput()},
			wantApplied: 1,
			checkApplied: func(t *testing.T, applied []appliedService) {
				t.Helper()
				target := applied[0].annotations["external-dns.alpha.kubernetes.io/target"]
				want := testAddrPair13
				if target != want {
					t.Errorf("target = %q, want %q", target, want)
				}
			},
		},
		{
			name: "nil Ready condition treated as ready",
			objects: []client.Object{
				makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
					{
						Addresses:  []string{testAddr1},
						Conditions: discoveryv1.EndpointConditions{Ready: nil},
					},
				}),
			},
			outputs:     []config.OutputConfig{defaultOutput()},
			wantApplied: 1,
			checkApplied: func(t *testing.T, applied []appliedService) {
				t.Helper()
				target := applied[0].annotations["external-dns.alpha.kubernetes.io/target"]
				if target != testAddr1 {
					t.Errorf("target = %q, want %q", target, testAddr1)
				}
			},
		},
		{
			name: "empty address list skips Service update for split-brain protection",
			objects: []client.Object{
				makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
					{
						Addresses:  []string{testAddr1},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(false)},
					},
				}),
			},
			outputs:     []config.OutputConfig{defaultOutput()},
			wantApplied: 0,
		},
		{
			name: "IPv4 filtering excludes IPv6 EndpointSlices",
			objects: []client.Object{
				makeEndpointSlice("eps-v4", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
					{
						Addresses:  []string{testAddr1},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
					},
				}),
				makeEndpointSlice("eps-v6", discoveryv1.AddressTypeIPv6, []discoveryv1.Endpoint{
					{
						Addresses:  []string{"fd00::1"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
					},
				}),
			},
			outputs:     []config.OutputConfig{defaultOutput()},
			wantApplied: 1,
			checkApplied: func(t *testing.T, applied []appliedService) {
				t.Helper()
				target := applied[0].annotations["external-dns.alpha.kubernetes.io/target"]
				if target != testAddr1 {
					t.Errorf("target = %q, want %q (should exclude IPv6)", target, testAddr1)
				}
			},
		},
		{
			name: "IPv6 filtering excludes IPv4 EndpointSlices",
			objects: []client.Object{
				makeEndpointSlice("eps-v4", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
					{
						Addresses:  []string{testAddr1},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
					},
				}),
				makeEndpointSlice("eps-v6", discoveryv1.AddressTypeIPv6, []discoveryv1.Endpoint{
					{
						Addresses:  []string{"fd00::1"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
					},
				}),
			},
			outputs: []config.OutputConfig{
				{
					Name:             "ipv6-output",
					Hostnames:        []string{testHostExample},
					AnnotationPrefix: "external-dns.alpha.kubernetes.io/",
					ServiceName:      "kuberture-v6",
					RecordTTL:        60,
					AddressSource:    config.AddressSourceEndpointSlice,
					AddressType:      config.AddressTypeIPv6,
				},
			},
			wantApplied: 1,
			checkApplied: func(t *testing.T, applied []appliedService) {
				t.Helper()
				target := applied[0].annotations["external-dns.alpha.kubernetes.io/target"]
				if target != "fd00::1" {
					t.Errorf("target = %q, want %q (should exclude IPv4)", target, "fd00::1")
				}
			},
		},
		{
			name: "multiple outputs create multiple Services",
			objects: []client.Object{
				makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
					{
						Addresses:  []string{testAddr1},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
					},
				}),
			},
			outputs: []config.OutputConfig{
				{
					Name:             "output-a",
					Hostnames:        []string{"a.example.com"},
					AnnotationPrefix: "external-dns.alpha.kubernetes.io/",
					ServiceName:      testSvcA,
					RecordTTL:        120,
					AddressSource:    config.AddressSourceEndpointSlice,
					AddressType:      config.AddressTypeIPv4,
				},
				{
					Name:             "output-b",
					Hostnames:        []string{"b.example.com"},
					AnnotationPrefix: "external-dns.alpha.kubernetes.io/",
					ServiceName:      testSvcB,
					RecordTTL:        300,
					AddressSource:    config.AddressSourceEndpointSlice,
					AddressType:      config.AddressTypeIPv4,
				},
			},
			wantApplied: 2,
			checkApplied: func(t *testing.T, applied []appliedService) {
				t.Helper()

				if applied[0].name != testSvcA {
					t.Errorf("first service name = %q, want %q", applied[0].name, testSvcA)
				}

				if applied[1].name != testSvcB {
					t.Errorf("second service name = %q, want %q", applied[1].name, testSvcB)
				}

				ttlA := applied[0].annotations["external-dns.alpha.kubernetes.io/ttl"]
				if ttlA != "120" {
					t.Errorf("first service ttl = %q, want %q", ttlA, "120")
				}

				ttlB := applied[1].annotations["external-dns.alpha.kubernetes.io/ttl"]
				if ttlB != "300" {
					t.Errorf("second service ttl = %q, want %q", ttlB, "300")
				}
			},
		},
		{
			name: "addressSource node-internal resolves InternalIP from Node",
			objects: []client.Object{
				makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.244.0.5"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
						NodeName:   strPtr("node-1"),
					},
				}),
				makeNode("node-1", []corev1.NodeAddress{
					{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
					{Type: corev1.NodeExternalIP, Address: testNodeExtAddr},
				}),
			},
			outputs: []config.OutputConfig{
				{
					Name:             "node-int",
					Hostnames:        []string{testHostExample},
					AnnotationPrefix: "external-dns.alpha.kubernetes.io/",
					ServiceName:      "kuberture-ni",
					RecordTTL:        60,
					AddressSource:    config.AddressSourceNodeInternal,
					AddressType:      config.AddressTypeIPv4,
				},
			},
			wantApplied: 1,
			checkApplied: func(t *testing.T, applied []appliedService) {
				t.Helper()
				target := applied[0].annotations["external-dns.alpha.kubernetes.io/target"]
				if target != "192.168.1.10" {
					t.Errorf("target = %q, want %q", target, "192.168.1.10")
				}
			},
		},
		{
			name: "addressSource node-external resolves ExternalIP from Node",
			objects: []client.Object{
				makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.244.0.5"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
						NodeName:   strPtr("node-1"),
					},
				}),
				makeNode("node-1", []corev1.NodeAddress{
					{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
					{Type: corev1.NodeExternalIP, Address: testNodeExtAddr},
				}),
			},
			outputs: []config.OutputConfig{
				{
					Name:             "node-ext",
					Hostnames:        []string{testHostExample},
					AnnotationPrefix: "external-dns.alpha.kubernetes.io/",
					ServiceName:      "kuberture-ne",
					RecordTTL:        60,
					AddressSource:    config.AddressSourceNodeExternal,
					AddressType:      config.AddressTypeIPv4,
				},
			},
			wantApplied: 1,
			checkApplied: func(t *testing.T, applied []appliedService) {
				t.Helper()
				target := applied[0].annotations["external-dns.alpha.kubernetes.io/target"]
				if target != testNodeExtAddr {
					t.Errorf("target = %q, want %q", target, testNodeExtAddr)
				}
			},
		},
		{
			name: "addressSource node-public resolves non-RFC1918 IP from Node",
			objects: []client.Object{
				makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.244.0.5"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
						NodeName:   strPtr("node-1"),
					},
				}),
				makeNode("node-1", []corev1.NodeAddress{
					{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
					{Type: corev1.NodeExternalIP, Address: "8.8.8.8"},
				}),
			},
			outputs: []config.OutputConfig{
				{
					Name:             "node-pub",
					Hostnames:        []string{testHostExample},
					AnnotationPrefix: "external-dns.alpha.kubernetes.io/",
					ServiceName:      "kuberture-np",
					RecordTTL:        60,
					AddressSource:    config.AddressSourceNodePublic,
					AddressType:      config.AddressTypeIPv4,
				},
			},
			wantApplied: 1,
			checkApplied: func(t *testing.T, applied []appliedService) {
				t.Helper()
				target := applied[0].annotations["external-dns.alpha.kubernetes.io/target"]
				if target != "8.8.8.8" {
					t.Errorf("target = %q, want %q", target, "8.8.8.8")
				}
			},
		},
		{
			name: "multiple hostnames produce comma-separated annotation",
			objects: []client.Object{
				makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
					{
						Addresses:  []string{testAddr1},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
					},
				}),
			},
			outputs: []config.OutputConfig{
				{
					Name:             "multi-host",
					Hostnames:        []string{"a.example.com", "b.example.com", "c.example.com"},
					AnnotationPrefix: "external-dns.alpha.kubernetes.io/",
					ServiceName:      "kuberture-mh",
					RecordTTL:        60,
					AddressSource:    config.AddressSourceEndpointSlice,
					AddressType:      config.AddressTypeIPv4,
				},
			},
			wantApplied: 1,
			checkApplied: func(t *testing.T, applied []appliedService) {
				t.Helper()
				hostname := applied[0].annotations["external-dns.alpha.kubernetes.io/hostname"]
				want := "a.example.com,b.example.com,c.example.com"
				if hostname != want {
					t.Errorf("hostname = %q, want %q", hostname, want)
				}
			},
		},
		{
			name: "Apply error propagates from reconcileOutput",
			objects: []client.Object{
				makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
					{
						Addresses:  []string{testAddr1},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
					},
				}),
			},
			outputs:  []config.OutputConfig{defaultOutput()},
			applyErr: errors.New("simulated apply failure"),
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var applied []appliedService

			cfgPtr := newTestConfig(tt.outputs)
			rec := buildTestReconciler(t, tt.objects, cfgPtr, &applied, tt.applyErr)

			result, err := rec.Reconcile(context.Background(), ctrl.Request{})

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if result.RequeueAfter != requeueInterval {
				t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, requeueInterval)
			}

			if len(applied) != tt.wantApplied {
				t.Fatalf("applied %d services, want %d", len(applied), tt.wantApplied)
			}

			if tt.checkApplied != nil {
				tt.checkApplied(t, applied)
			}
		})
	}
}

func TestReconcile_ErrorListingEndpointSlices(t *testing.T) {
	scheme := newTestScheme(t)

	listErr := errors.New("simulated list failure")

	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(
				_ context.Context,
				_ client.WithWatch,
				_ client.ObjectList,
				_ ...client.ListOption,
			) error {
				return listErr
			},
		}).
		Build()

	output := defaultOutput()
	cfgPtr := newTestConfig([]config.OutputConfig{output})
	res := resolver.NewResolver(cli, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	log := slog.Default()

	rec := NewReconciler(cli, res, cfgPtr, log, "test-instance", testSvcNamespace, nil, nil)

	_, err := rec.Reconcile(context.Background(), ctrl.Request{})
	if err == nil {
		t.Fatal("expected error from List, got nil")
	}

	if !strings.Contains(err.Error(), "listing endpoint slices") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "listing endpoint slices")
	}
}

func TestReadyzCheck(t *testing.T) {
	tests := []struct {
		name           string
		runReconcile   bool
		wantErr        bool
		wantErrMessage string
	}{
		{
			name:           "returns error before first reconcile",
			runReconcile:   false,
			wantErr:        true,
			wantErrMessage: "controller has not completed initial reconciliation",
		},
		{
			name:         "returns nil after successful reconcile",
			runReconcile: true,
			wantErr:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output := defaultOutput()
			objects := []client.Object{
				makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
					{
						Addresses:  []string{testAddr1},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
					},
				}),
			}

			var applied []appliedService
			cfgPtr := newTestConfig([]config.OutputConfig{output})
			rec := buildTestReconciler(t, objects, cfgPtr, &applied, nil)

			if tt.runReconcile {
				_, err := rec.Reconcile(context.Background(), ctrl.Request{})
				if err != nil {
					t.Fatalf("unexpected reconcile error: %v", err)
				}
			}

			err := rec.ReadyzCheck(&http.Request{})

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if !strings.Contains(err.Error(), tt.wantErrMessage) {
					t.Errorf("error = %q, want it to contain %q", err.Error(), tt.wantErrMessage)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestReadyzCheck_BecomesUnhealthyAfterStaleTimeout(t *testing.T) {
	eps := makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
		{
			Addresses:  []string{testAddr1},
			Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
		},
	})

	output := defaultOutput()
	cfgPtr := newTestConfig([]config.OutputConfig{output})

	applyFn := func(
		_ context.Context,
		_ client.WithWatch,
		_ runtime.ApplyConfiguration,
		_ ...client.ApplyOption,
	) error {
		return nil
	}

	rec := buildTestReconcilerWithInterceptor(
		t,
		[]client.Object{eps},
		cfgPtr,
		interceptor.Funcs{Apply: applyFn},
	)

	// Successful reconcile.
	_, err := rec.Reconcile(context.Background(), ctrl.Request{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should be healthy right after.
	if checkErr := rec.ReadyzCheck(&http.Request{}); checkErr != nil {
		t.Fatalf("expected healthy after success, got: %v", checkErr)
	}

	// Simulate staleness by backdating lastSuccess.
	rec.mu.Lock()
	rec.lastSuccess = time.Now().Add(-stalenessThreshold - time.Minute)
	rec.mu.Unlock()

	checkErr := rec.ReadyzCheck(&http.Request{})
	if checkErr == nil {
		t.Fatal("expected unhealthy after stale timeout, got nil")
	}

	if !strings.Contains(checkErr.Error(), "stale") {
		t.Errorf("error = %q, want it to contain %q", checkErr.Error(), "stale")
	}
}

func TestReadyzCheck_RecoverAfterError(t *testing.T) {
	eps := makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
		{
			Addresses:  []string{testAddr1},
			Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
		},
	})

	output := defaultOutput()
	cfgPtr := newTestConfig([]config.OutputConfig{output})

	failOnce := true

	applyFn := func(
		_ context.Context,
		_ client.WithWatch,
		_ runtime.ApplyConfiguration,
		_ ...client.ApplyOption,
	) error {
		if failOnce {
			failOnce = false
			return errors.New("simulated failure")
		}
		return nil
	}

	rec := buildTestReconcilerWithInterceptor(
		t,
		[]client.Object{eps},
		cfgPtr,
		interceptor.Funcs{Apply: applyFn},
	)

	// First reconcile fails.
	_, err := rec.Reconcile(context.Background(), ctrl.Request{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	// Second reconcile succeeds.
	_, err = rec.Reconcile(context.Background(), ctrl.Request{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should be healthy after recovery.
	if checkErr := rec.ReadyzCheck(&http.Request{}); checkErr != nil {
		t.Fatalf("expected healthy after recovery, got: %v", checkErr)
	}
}

func TestBuildService(t *testing.T) {
	tests := []struct {
		name              string
		output            config.OutputConfig
		addresses         []string
		wantName          string
		wantNamespace     string
		wantHostname      string
		wantTarget        string
		wantTTL           string
		wantAnnotationLen int
	}{
		{
			name: "standard output with single address",
			output: config.OutputConfig{
				Name:             "test",
				Hostnames:        []string{testHostExample},
				AnnotationPrefix: "external-dns.alpha.kubernetes.io/",
				ServiceName:      "my-svc",
				RecordTTL:        300,
			},
			addresses:         []string{testAddr1},
			wantName:          "my-svc",
			wantNamespace:     testSvcNamespace,
			wantHostname:      testHostExample,
			wantTarget:        testAddr1,
			wantTTL:           "300",
			wantAnnotationLen: 3,
		},
		{
			name: "multiple addresses joined with commas",
			output: config.OutputConfig{
				Name:             "multi",
				Hostnames:        []string{"foo.bar"},
				AnnotationPrefix: "dns.test.io/",
				ServiceName:      "svc-multi",
				RecordTTL:        120,
			},
			addresses:         []string{testAddr1, "10.0.0.2", "10.0.0.3"},
			wantName:          "svc-multi",
			wantNamespace:     testSvcNamespace,
			wantHostname:      "foo.bar",
			wantTarget:        "10.0.0.1,10.0.0.2,10.0.0.3",
			wantTTL:           "120",
			wantAnnotationLen: 3,
		},
		{
			name: "multiple hostnames joined with commas",
			output: config.OutputConfig{
				Name:             "multi-host",
				Hostnames:        []string{"a.com", "b.com"},
				AnnotationPrefix: "external-dns.alpha.kubernetes.io/",
				ServiceName:      "svc-hosts",
				RecordTTL:        60,
			},
			addresses:         []string{testAddr1},
			wantName:          "svc-hosts",
			wantNamespace:     testSvcNamespace,
			wantHostname:      "a.com,b.com",
			wantTarget:        testAddr1,
			wantTTL:           "60",
			wantAnnotationLen: 3,
		},
		{
			name: "custom annotation prefix",
			output: config.OutputConfig{
				Name:             "custom-prefix",
				Hostnames:        []string{"test.io"},
				AnnotationPrefix: "my-dns.io/",
				ServiceName:      "svc-custom",
				RecordTTL:        60,
			},
			addresses:     []string{testAddr1},
			wantName:      "svc-custom",
			wantNamespace: testSvcNamespace,
			wantHostname:  "test.io",
			wantTarget:    testAddr1,
			wantTTL:       "60",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &Reconciler{namespace: testSvcNamespace}
			svc := rec.buildService(&tt.output, tt.addresses)

			if svc.ObjectMetaApplyConfiguration == nil {
				t.Fatal("ObjectMeta is nil")
			}

			if *svc.Name != tt.wantName {
				t.Errorf("name = %q, want %q", *svc.Name, tt.wantName)
			}

			if *svc.Namespace != tt.wantNamespace {
				t.Errorf("namespace = %q, want %q", *svc.Namespace, tt.wantNamespace)
			}

			prefix := tt.output.AnnotationPrefix

			gotHostname := svc.Annotations[prefix+"hostname"]
			if gotHostname != tt.wantHostname {
				t.Errorf("hostname annotation = %q, want %q", gotHostname, tt.wantHostname)
			}

			gotTarget := svc.Annotations[prefix+"target"]
			if gotTarget != tt.wantTarget {
				t.Errorf("target annotation = %q, want %q", gotTarget, tt.wantTarget)
			}

			gotTTL := svc.Annotations[prefix+"ttl"]
			if gotTTL != tt.wantTTL {
				t.Errorf("ttl annotation = %q, want %q", gotTTL, tt.wantTTL)
			}

			// Verify TTL is a valid integer.
			_, parseErr := strconv.Atoi(gotTTL)
			if parseErr != nil {
				t.Errorf("ttl annotation %q is not a valid integer: %v", gotTTL, parseErr)
			}

			if svc.Spec == nil {
				t.Fatal("Spec is nil")
			}

			if *svc.Spec.Type != corev1.ServiceTypeClusterIP {
				t.Errorf("service type = %v, want ClusterIP", *svc.Spec.Type)
			}

			if *svc.Spec.ClusterIP != string(corev1.ClusterIPNone) {
				t.Errorf("clusterIP = %q, want %q", *svc.Spec.ClusterIP, corev1.ClusterIPNone)
			}
		})
	}
}

func TestReconcile_NoEndpointSlices(t *testing.T) {
	output := defaultOutput()
	var applied []appliedService
	cfgPtr := newTestConfig([]config.OutputConfig{output})

	rec := buildTestReconciler(t, nil, cfgPtr, &applied, nil)

	_, err := rec.Reconcile(context.Background(), ctrl.Request{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(applied) != 0 {
		t.Errorf("applied %d services, want 0 (split-brain protection)", len(applied))
	}
}

func TestNewTriggerRequest(t *testing.T) {
	req := newTriggerRequest()

	if req.Name != "trigger" {
		t.Errorf("trigger request name = %q, want %q", req.Name, "trigger")
	}

	if req.Namespace != testNamespace {
		t.Errorf("trigger request namespace = %q, want %q", req.Namespace, testNamespace)
	}
}

func TestNewReconciler(t *testing.T) {
	scheme := newTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(scheme).Build()
	res := resolver.NewResolver(cli, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	log := slog.Default()

	cfgPtr := newTestConfig([]config.OutputConfig{defaultOutput()})

	rec := NewReconciler(cli, res, cfgPtr, log, "test-instance", testSvcNamespace, nil, nil)

	if rec == nil {
		t.Fatal("NewReconciler returned nil")
	}

	if rec.client != cli {
		t.Error("client not set correctly")
	}

	if rec.resolver != res {
		t.Error("resolver not set correctly")
	}

	if rec.config != cfgPtr {
		t.Error("config not set correctly")
	}

	if rec.instanceName != "test-instance" {
		t.Errorf("instanceName = %q, want %q", rec.instanceName, "test-instance")
	}

	if rec.namespace != testSvcNamespace {
		t.Errorf("namespace = %q, want %q", rec.namespace, testSvcNamespace)
	}

	if !rec.lastSuccess.IsZero() {
		t.Error("reconciler should not be ready initially")
	}
}

// buildTestReconcilerWithInterceptor creates a Reconciler whose fake client
// uses the caller-supplied interceptor functions, allowing fine-grained
// control over which operations succeed or fail.
func buildTestReconcilerWithInterceptor(
	t *testing.T,
	initObjs []client.Object,
	cfgPtr *atomic.Pointer[config.Config],
	funcs interceptor.Funcs,
) *Reconciler {
	t.Helper()

	scheme := newTestScheme(t)

	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(initObjs...).
		WithInterceptorFuncs(funcs).
		Build()

	res := resolver.NewResolver(cli, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	log := slog.Default()

	return NewReconciler(cli, res, cfgPtr, log, "test-instance", testSvcNamespace, nil, nil)
}

func TestReconcile_PartialOutputFailure(t *testing.T) {
	eps := makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
		{
			Addresses:  []string{testAddr1},
			Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
		},
	})

	outputA := config.OutputConfig{
		Name:             "output-a",
		Hostnames:        []string{"a.example.com"},
		AnnotationPrefix: "external-dns.alpha.kubernetes.io/",
		ServiceName:      testSvcA,
		RecordTTL:        60,
		AddressSource:    config.AddressSourceEndpointSlice,
		AddressType:      config.AddressTypeIPv4,
	}

	outputB := config.OutputConfig{
		Name:             "output-b",
		Hostnames:        []string{"b.example.com"},
		AnnotationPrefix: "external-dns.alpha.kubernetes.io/",
		ServiceName:      testSvcB,
		RecordTTL:        60,
		AddressSource:    config.AddressSourceEndpointSlice,
		AddressType:      config.AddressTypeIPv4,
	}

	cfgPtr := newTestConfig([]config.OutputConfig{outputA, outputB})

	var applied []appliedService

	callCount := 0

	applyFn := func(
		_ context.Context,
		_ client.WithWatch,
		obj runtime.ApplyConfiguration,
		_ ...client.ApplyOption,
	) error {
		callCount++

		svc, ok := obj.(*applycorev1.ServiceApplyConfiguration)
		if !ok {
			t.Fatalf("Apply called with non-Service object: %T", obj)
		}

		// Fail on the first Apply call (output-a).
		if *svc.Name == testSvcA {
			return errors.New("simulated apply failure for svc-a")
		}

		rec := appliedService{}

		if svc.Name != nil {
			rec.name = *svc.Name
		}

		if svc.Namespace != nil {
			rec.namespace = *svc.Namespace
		}

		if svc.Annotations != nil {
			rec.annotations = svc.Annotations
		}

		applied = append(applied, rec)

		return nil
	}

	rec := buildTestReconcilerWithInterceptor(
		t,
		[]client.Object{eps},
		cfgPtr,
		interceptor.Funcs{Apply: applyFn},
	)

	_, err := rec.Reconcile(context.Background(), ctrl.Request{})
	if err == nil {
		t.Fatal("expected error from partial output failure, got nil")
	}

	if !strings.Contains(err.Error(), "output-a") {
		t.Errorf("error = %q, want it to mention output-a", err.Error())
	}

	// Both Apply calls should have been attempted.
	if callCount != 2 {
		t.Errorf("Apply called %d times, want 2", callCount)
	}

	// Second output should have succeeded.
	if len(applied) != 1 {
		t.Fatalf("applied %d services, want 1 (svc-b)", len(applied))
	}

	if applied[0].name != testSvcB {
		t.Errorf("applied service name = %q, want %q", applied[0].name, testSvcB)
	}
}

func TestReconcile_CustomAnnotationPrefix(t *testing.T) {
	eps := makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
		{
			Addresses:  []string{testAddr1},
			Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
		},
	})

	output := config.OutputConfig{
		Name:             "custom-prefix",
		Hostnames:        []string{"test.io"},
		AnnotationPrefix: "my-dns.io/",
		ServiceName:      "svc-custom",
		RecordTTL:        60,
		AddressSource:    config.AddressSourceEndpointSlice,
		AddressType:      config.AddressTypeIPv4,
	}

	var applied []appliedService
	cfgPtr := newTestConfig([]config.OutputConfig{output})
	rec := buildTestReconciler(t, []client.Object{eps}, cfgPtr, &applied, nil)

	_, err := rec.Reconcile(context.Background(), ctrl.Request{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(applied) != 1 {
		t.Fatalf("applied %d services, want 1", len(applied))
	}

	svc := applied[0]

	wantKeys := []string{"my-dns.io/hostname", "my-dns.io/target", "my-dns.io/ttl"}

	for _, key := range wantKeys {
		if _, found := svc.annotations[key]; !found {
			t.Errorf("annotation %q not found", key)
		}
	}

	if svc.annotations["my-dns.io/hostname"] != "test.io" {
		t.Errorf("hostname = %q, want %q", svc.annotations["my-dns.io/hostname"], "test.io")
	}

	if svc.annotations["my-dns.io/target"] != testAddr1 {
		t.Errorf("target = %q, want %q", svc.annotations["my-dns.io/target"], testAddr1)
	}

	if svc.annotations["my-dns.io/ttl"] != "60" {
		t.Errorf("ttl = %q, want %q", svc.annotations["my-dns.io/ttl"], "60")
	}
}

func TestReconcile_ConfigHotReload(t *testing.T) {
	eps := makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
		{
			Addresses:  []string{testAddr1},
			Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
		},
	})

	configA := &config.Config{
		Source: config.SourceConfig{
			Namespace:   testNamespace,
			ServiceName: "kubernetes",
		},
		Outputs: []config.OutputConfig{
			{
				Name:             "hot-reload",
				Hostnames:        []string{"a.com"},
				AnnotationPrefix: "external-dns.alpha.kubernetes.io/",
				ServiceName:      "svc-reload",
				RecordTTL:        60,
				AddressSource:    config.AddressSourceEndpointSlice,
				AddressType:      config.AddressTypeIPv4,
			},
		},
	}

	cfgPtr := &atomic.Pointer[config.Config]{}
	cfgPtr.Store(configA)

	var applied []appliedService
	rec := buildTestReconciler(t, []client.Object{eps}, cfgPtr, &applied, nil)

	// First reconcile with config A.
	_, err := rec.Reconcile(context.Background(), ctrl.Request{})
	if err != nil {
		t.Fatalf("first reconcile error: %v", err)
	}

	if len(applied) != 1 {
		t.Fatalf("first reconcile: applied %d, want 1", len(applied))
	}

	hostname := applied[0].annotations["external-dns.alpha.kubernetes.io/hostname"]
	if hostname != "a.com" {
		t.Errorf("first reconcile: hostname = %q, want %q", hostname, "a.com")
	}

	// Swap to config B.
	configB := &config.Config{
		Source: config.SourceConfig{
			Namespace:   testNamespace,
			ServiceName: "kubernetes",
		},
		Outputs: []config.OutputConfig{
			{
				Name:             "hot-reload",
				Hostnames:        []string{"b.com"},
				AnnotationPrefix: "external-dns.alpha.kubernetes.io/",
				ServiceName:      "svc-reload",
				RecordTTL:        60,
				AddressSource:    config.AddressSourceEndpointSlice,
				AddressType:      config.AddressTypeIPv4,
			},
		},
	}

	cfgPtr.Store(configB)

	// Second reconcile should use the new config.
	_, err = rec.Reconcile(context.Background(), ctrl.Request{})
	if err != nil {
		t.Fatalf("second reconcile error: %v", err)
	}

	if len(applied) != 2 {
		t.Fatalf("after second reconcile: applied %d, want 2", len(applied))
	}

	hostname = applied[1].annotations["external-dns.alpha.kubernetes.io/hostname"]
	if hostname != "b.com" {
		t.Errorf("second reconcile: hostname = %q, want %q", hostname, "b.com")
	}
}

func TestReconcile_EndpointSliceDifferentNamespace(t *testing.T) {
	// Create an EndpointSlice in a different namespace.
	eps := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "eps-other-ns",
			Namespace: "kube-system",
			Labels: map[string]string{
				discoveryv1.LabelServiceName: "kubernetes",
			},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{
			{
				Addresses:  []string{testAddr1},
				Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
			},
		},
	}

	output := defaultOutput()

	var applied []appliedService
	cfgPtr := newTestConfig([]config.OutputConfig{output})
	rec := buildTestReconciler(t, []client.Object{eps}, cfgPtr, &applied, nil)

	_, err := rec.Reconcile(context.Background(), ctrl.Request{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// EndpointSlice is in kube-system, source config watches "default".
	// List filters by namespace, so no addresses should be resolved.
	if len(applied) != 0 {
		t.Errorf("applied %d services, want 0 (different namespace)", len(applied))
	}
}

func TestReconcile_EndpointSliceDifferentServiceLabel(t *testing.T) {
	// Create an EndpointSlice with a different service label.
	eps := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "eps-wrong-svc",
			Namespace: testNamespace,
			Labels: map[string]string{
				discoveryv1.LabelServiceName: "other-service",
			},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{
			{
				Addresses:  []string{testAddr1},
				Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
			},
		},
	}

	output := defaultOutput()

	var applied []appliedService
	cfgPtr := newTestConfig([]config.OutputConfig{output})
	rec := buildTestReconciler(t, []client.Object{eps}, cfgPtr, &applied, nil)

	_, err := rec.Reconcile(context.Background(), ctrl.Request{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Source config watches "kubernetes" service, but EndpointSlice labels "other-service".
	if len(applied) != 0 {
		t.Errorf("applied %d services, want 0 (wrong service label)", len(applied))
	}
}

func TestReconcile_EmptyOutputs(t *testing.T) {
	eps := makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
		{
			Addresses:  []string{testAddr1},
			Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
		},
	})

	// Config with zero outputs.
	cfg := &config.Config{
		Source: config.SourceConfig{
			Namespace:   testNamespace,
			ServiceName: "kubernetes",
		},
		Outputs: []config.OutputConfig{},
	}

	cfgPtr := &atomic.Pointer[config.Config]{}
	cfgPtr.Store(cfg)

	var applied []appliedService
	rec := buildTestReconciler(t, []client.Object{eps}, cfgPtr, &applied, nil)

	_, err := rec.Reconcile(context.Background(), ctrl.Request{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(applied) != 0 {
		t.Errorf("applied %d services, want 0 (no outputs)", len(applied))
	}
}

func TestReconcile_LargeNumberOfAddresses(t *testing.T) {
	const addressCount = 100

	endpoints := make([]discoveryv1.Endpoint, 0, addressCount)

	for idx := range addressCount {
		endpoints = append(endpoints, discoveryv1.Endpoint{
			Addresses:  []string{fmt.Sprintf("10.0.%d.%d", idx/256, idx%256)},
			Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
		})
	}

	eps := makeEndpointSlice("eps-large", discoveryv1.AddressTypeIPv4, endpoints)
	output := defaultOutput()

	var applied []appliedService
	cfgPtr := newTestConfig([]config.OutputConfig{output})
	rec := buildTestReconciler(t, []client.Object{eps}, cfgPtr, &applied, nil)

	_, err := rec.Reconcile(context.Background(), ctrl.Request{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(applied) != 1 {
		t.Fatalf("applied %d services, want 1", len(applied))
	}

	target := applied[0].annotations["external-dns.alpha.kubernetes.io/target"]
	parts := strings.Split(target, ",")

	if len(parts) != addressCount {
		t.Errorf("target has %d addresses, want %d", len(parts), addressCount)
	}

	// Verify addresses are sorted.
	if !sort.StringsAreSorted(parts) {
		t.Error("target addresses are not sorted")
	}
}

func TestReconcile_SuccessSetsMetrics(t *testing.T) {
	eps := makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
		{
			Addresses:  []string{testAddr1},
			Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
		},
	})

	output := defaultOutput()

	var applied []appliedService
	cfgPtr := newTestConfig([]config.OutputConfig{output})
	rec := buildTestReconciler(t, []client.Object{eps}, cfgPtr, &applied, nil)

	// Run reconcile multiple times and verify ready stays true.
	for idx := range 3 {
		_, err := rec.Reconcile(context.Background(), ctrl.Request{})
		if err != nil {
			t.Fatalf("reconcile %d: unexpected error: %v", idx, err)
		}

		rec.mu.Lock()
		ready := !rec.lastSuccess.IsZero()
		rec.mu.Unlock()

		if !ready {
			t.Errorf("reconcile %d: ready = false, want true", idx)
		}
	}
}

func TestReconcile_RecordsDuration(t *testing.T) {
	eps := makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
		{
			Addresses:  []string{testAddr1},
			Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
		},
	})

	output := defaultOutput()

	var applied []appliedService
	cfgPtr := newTestConfig([]config.OutputConfig{output})
	rec := buildTestReconciler(t, []client.Object{eps}, cfgPtr, &applied, nil)

	_, err := rec.Reconcile(context.Background(), ctrl.Request{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	count := testutil.CollectAndCount(reconcileDuration)
	if count == 0 {
		t.Error("reconcileDuration histogram has no observations after successful reconcile")
	}
}

func TestReconcile_NodeInternalWithIPFallback(t *testing.T) {
	// Endpoint without nodeName. The resolver should fall back to
	// matching the endpoint IP against node addresses.
	eps := makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
		{
			Addresses:  []string{"192.168.1.50"},
			Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
			// No NodeName set.
		},
	})

	node := makeNode("node-1", []corev1.NodeAddress{
		{Type: corev1.NodeInternalIP, Address: "192.168.1.50"},
		{Type: corev1.NodeExternalIP, Address: "203.0.113.50"},
	})

	output := config.OutputConfig{
		Name:             "ip-fallback",
		Hostnames:        []string{testHostExample},
		AnnotationPrefix: "external-dns.alpha.kubernetes.io/",
		ServiceName:      "svc-fallback",
		RecordTTL:        60,
		AddressSource:    config.AddressSourceNodeInternal,
		AddressType:      config.AddressTypeIPv4,
	}

	var applied []appliedService
	cfgPtr := newTestConfig([]config.OutputConfig{output})
	rec := buildTestReconciler(t, []client.Object{eps, node}, cfgPtr, &applied, nil)

	_, err := rec.Reconcile(context.Background(), ctrl.Request{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(applied) != 1 {
		t.Fatalf("applied %d services, want 1", len(applied))
	}

	target := applied[0].annotations["external-dns.alpha.kubernetes.io/target"]
	if target != "192.168.1.50" {
		t.Errorf("target = %q, want %q", target, "192.168.1.50")
	}
}

func TestReconcile_MultipleOutputsDifferentSources(t *testing.T) {
	eps := makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
		{
			Addresses:  []string{testAddr1},
			Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
			NodeName:   strPtr("node-1"),
		},
	})

	node := makeNode("node-1", []corev1.NodeAddress{
		{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
		{Type: corev1.NodeExternalIP, Address: testNodeExtAddr},
	})

	outputEPS := config.OutputConfig{
		Name:             "output-eps",
		Hostnames:        []string{"eps.example.com"},
		AnnotationPrefix: "external-dns.alpha.kubernetes.io/",
		ServiceName:      "svc-eps",
		RecordTTL:        60,
		AddressSource:    config.AddressSourceEndpointSlice,
		AddressType:      config.AddressTypeIPv4,
	}

	outputNodeExt := config.OutputConfig{
		Name:             "output-node-ext",
		Hostnames:        []string{"ext.example.com"},
		AnnotationPrefix: "external-dns.alpha.kubernetes.io/",
		ServiceName:      "svc-node-ext",
		RecordTTL:        60,
		AddressSource:    config.AddressSourceNodeExternal,
		AddressType:      config.AddressTypeIPv4,
	}

	var applied []appliedService
	cfgPtr := newTestConfig([]config.OutputConfig{outputEPS, outputNodeExt})
	rec := buildTestReconciler(t, []client.Object{eps, node}, cfgPtr, &applied, nil)

	_, err := rec.Reconcile(context.Background(), ctrl.Request{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(applied) != 2 {
		t.Fatalf("applied %d services, want 2", len(applied))
	}

	// First output uses endpointslice source.
	targetEPS := applied[0].annotations["external-dns.alpha.kubernetes.io/target"]
	if targetEPS != testAddr1 {
		t.Errorf("EPS target = %q, want %q", targetEPS, testAddr1)
	}

	// Second output uses node-external source.
	targetNode := applied[1].annotations["external-dns.alpha.kubernetes.io/target"]
	if targetNode != testNodeExtAddr {
		t.Errorf("node-external target = %q, want %q", targetNode, testNodeExtAddr)
	}
}

func TestReconcile_ResolverError(t *testing.T) {
	// Endpoint without nodeName so resolver falls back to IP matching via List nodes.
	eps := makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
		{
			Addresses:  []string{"10.244.0.5"},
			Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
		},
	})

	output := config.OutputConfig{
		Name:             "resolver-err",
		Hostnames:        []string{testHostExample},
		AnnotationPrefix: "external-dns.alpha.kubernetes.io/",
		ServiceName:      "svc-resolver-err",
		RecordTTL:        60,
		AddressSource:    config.AddressSourceNodeInternal,
		AddressType:      config.AddressTypeIPv4,
	}

	cfgPtr := newTestConfig([]config.OutputConfig{output})

	listErr := errors.New("simulated node list failure")

	listFn := func(
		ctx context.Context,
		underlying client.WithWatch,
		obj client.ObjectList,
		opts ...client.ListOption,
	) error {
		// Fail only on node list; delegate EndpointSlice list to underlying client.
		if _, isNodeList := obj.(*corev1.NodeList); isNodeList {
			return listErr
		}

		return underlying.List(ctx, obj, opts...)
	}

	rec := buildTestReconcilerWithInterceptor(
		t,
		[]client.Object{eps},
		cfgPtr,
		interceptor.Funcs{List: listFn},
	)

	_, err := rec.Reconcile(context.Background(), ctrl.Request{})
	if err == nil {
		t.Fatal("expected error from resolver, got nil")
	}

	if !strings.Contains(err.Error(), "resolving addresses") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "resolving addresses")
	}
}

func TestReadyzCheck_ConcurrentAccess(t *testing.T) {
	eps := makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
		{
			Addresses:  []string{testAddr1},
			Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
		},
	})

	output := defaultOutput()
	cfgPtr := newTestConfig([]config.OutputConfig{output})

	// Use a mutex-protected no-op Apply to avoid data races on the
	// applied slice, since this test only cares about ReadyzCheck safety.
	applyFn := func(
		_ context.Context,
		_ client.WithWatch,
		_ runtime.ApplyConfiguration,
		_ ...client.ApplyOption,
	) error {
		return nil
	}

	rec := buildTestReconcilerWithInterceptor(
		t,
		[]client.Object{eps},
		cfgPtr,
		interceptor.Funcs{Apply: applyFn},
	)

	const goroutines = 50

	var waitGroup sync.WaitGroup

	waitGroup.Add(goroutines)

	// Run ReadyzCheck and Reconcile concurrently.
	for idx := range goroutines {
		go func(iteration int) {
			defer waitGroup.Done()

			if iteration%2 == 0 {
				_ = rec.ReadyzCheck(&http.Request{})
			} else {
				_, _ = rec.Reconcile(context.Background(), ctrl.Request{})
			}
		}(idx)
	}

	waitGroup.Wait()

	// If the race detector did not fire, the test passes.
	// Verify final state is ready after concurrent reconciles.
	err := rec.ReadyzCheck(&http.Request{})
	if err != nil {
		t.Errorf("expected nil after concurrent reconciles, got: %v", err)
	}
}

func TestBuildService_ZeroTTL(t *testing.T) {
	output := config.OutputConfig{
		Name:             "zero-ttl",
		Hostnames:        []string{testHostExample},
		AnnotationPrefix: "external-dns.alpha.kubernetes.io/",
		ServiceName:      "svc-zero-ttl",
		RecordTTL:        0,
	}

	rec := &Reconciler{}
	svc := rec.buildService(&output, []string{testAddr1})

	prefix := output.AnnotationPrefix

	gotTTL := svc.Annotations[prefix+"ttl"]
	if gotTTL != "0" {
		t.Errorf("ttl = %q, want %q", gotTTL, "0")
	}

	// Verify it parses as a valid integer.
	val, parseErr := strconv.Atoi(gotTTL)
	if parseErr != nil {
		t.Errorf("ttl %q is not a valid integer: %v", gotTTL, parseErr)
	}

	if val != 0 {
		t.Errorf("parsed ttl = %d, want 0", val)
	}
}

func TestBuildService_SingleHostname(t *testing.T) {
	output := config.OutputConfig{
		Name:             "single-host",
		Hostnames:        []string{"only.example.com"},
		AnnotationPrefix: "external-dns.alpha.kubernetes.io/",
		ServiceName:      "svc-single",
		RecordTTL:        60,
	}

	rec := &Reconciler{}
	svc := rec.buildService(&output, []string{testAddr1})

	prefix := output.AnnotationPrefix

	gotHostname := svc.Annotations[prefix+"hostname"]
	if gotHostname != "only.example.com" {
		t.Errorf("hostname = %q, want %q", gotHostname, "only.example.com")
	}

	// Verify no trailing comma.
	if strings.HasSuffix(gotHostname, ",") {
		t.Errorf("hostname %q has trailing comma", gotHostname)
	}

	// Verify no leading comma.
	if strings.HasPrefix(gotHostname, ",") {
		t.Errorf("hostname %q has leading comma", gotHostname)
	}
}

func TestBuildService_EmptyAddresses(t *testing.T) {
	output := config.OutputConfig{
		Name:             "empty-addr",
		Hostnames:        []string{testHostExample},
		AnnotationPrefix: "external-dns.alpha.kubernetes.io/",
		ServiceName:      "svc-empty",
		RecordTTL:        60,
	}

	rec := &Reconciler{}
	svc := rec.buildService(&output, []string{})

	prefix := output.AnnotationPrefix

	gotTarget := svc.Annotations[prefix+"target"]
	if gotTarget != "" {
		t.Errorf("target = %q, want empty string", gotTarget)
	}

	// Verify other annotations are still present.
	gotHostname := svc.Annotations[prefix+"hostname"]
	if gotHostname != testHostExample {
		t.Errorf("hostname = %q, want %q", gotHostname, testHostExample)
	}

	gotTTL := svc.Annotations[prefix+"ttl"]
	if gotTTL != "60" {
		t.Errorf("ttl = %q, want %q", gotTTL, "60")
	}
}

func TestBuildService_ManagedByLabel(t *testing.T) {
	output := config.OutputConfig{
		Name:             "label-test",
		Hostnames:        []string{testHostExample},
		AnnotationPrefix: "external-dns.alpha.kubernetes.io/",
		ServiceName:      "svc-label",
		RecordTTL:        60,
	}

	rec := &Reconciler{}
	svc := rec.buildService(&output, []string{testAddr1})

	if svc.Labels == nil {
		t.Fatal("Labels is nil, expected managed-by label")
	}

	got := svc.Labels[managedByLabel]
	if got != managedByValue {
		t.Errorf("managed-by label = %q, want %q", got, managedByValue)
	}
}

func TestBuildService_WithOwnerRef(t *testing.T) {
	ownerRef := &metav1.OwnerReference{
		APIVersion:         "apps/v1",
		Kind:               "Deployment",
		Name:               "kuberture",
		UID:                types.UID("test-uid-12345"),
		BlockOwnerDeletion: boolPtr(true),
	}

	output := config.OutputConfig{
		Name:             "owner-ref-test",
		Hostnames:        []string{testHostExample},
		AnnotationPrefix: "external-dns.alpha.kubernetes.io/",
		ServiceName:      "svc-owner",
		RecordTTL:        60,
	}

	rec := &Reconciler{namespace: testSvcNamespace, ownerRef: ownerRef}
	svc := rec.buildService(&output, []string{testAddr1})

	if svc.ObjectMetaApplyConfiguration == nil {
		t.Fatal("ObjectMeta is nil")
	}

	if len(svc.OwnerReferences) != 1 {
		t.Fatalf("OwnerReferences length = %d, want 1", len(svc.OwnerReferences))
	}

	ref := svc.OwnerReferences[0]

	if ref.APIVersion == nil || *ref.APIVersion != "apps/v1" {
		t.Errorf("OwnerReference APIVersion = %v, want %q", ref.APIVersion, "apps/v1")
	}

	if ref.Kind == nil || *ref.Kind != "Deployment" {
		t.Errorf("OwnerReference Kind = %v, want %q", ref.Kind, "Deployment")
	}

	if ref.Name == nil || *ref.Name != "kuberture" {
		t.Errorf("OwnerReference Name = %v, want %q", ref.Name, "kuberture")
	}

	if ref.UID == nil || *ref.UID != types.UID("test-uid-12345") {
		t.Errorf("OwnerReference UID = %v, want %q", ref.UID, "test-uid-12345")
	}

	if ref.BlockOwnerDeletion == nil || !*ref.BlockOwnerDeletion {
		t.Errorf("OwnerReference BlockOwnerDeletion = %v, want true", ref.BlockOwnerDeletion)
	}
}

func TestBuildService_WithoutOwnerRef(t *testing.T) {
	output := config.OutputConfig{
		Name:             "no-owner-ref-test",
		Hostnames:        []string{testHostExample},
		AnnotationPrefix: "external-dns.alpha.kubernetes.io/",
		ServiceName:      "svc-no-owner",
		RecordTTL:        60,
	}

	rec := &Reconciler{namespace: testSvcNamespace, ownerRef: nil}
	svc := rec.buildService(&output, []string{testAddr1})

	if len(svc.OwnerReferences) != 0 {
		t.Errorf("OwnerReferences length = %d, want 0 when ownerRef is nil", len(svc.OwnerReferences))
	}
}

func TestNewReconciler_WithOwnerRef(t *testing.T) {
	scheme := newTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(scheme).Build()
	res := resolver.NewResolver(cli, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	log := slog.Default()

	cfgPtr := newTestConfig([]config.OutputConfig{defaultOutput()})

	ownerRef := &metav1.OwnerReference{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       "kuberture",
		UID:        types.UID("test-uid-456"),
	}

	rec := NewReconciler(cli, res, cfgPtr, log, "test-instance", testSvcNamespace, ownerRef, nil)

	if rec.ownerRef != ownerRef {
		t.Error("ownerRef not set correctly")
	}
}

// deletedService records what was passed to the intercepted Delete call.
type deletedService struct {
	name      string
	namespace string
}

func TestReconcile_StaleServiceCleanup(t *testing.T) {
	eps := makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
		{
			Addresses:  []string{testAddr1},
			Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
		},
	})

	// Simulate a previously-created Service that is no longer in the config.
	staleService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "stale-svc",
			Namespace: testSvcNamespace,
			Labels: map[string]string{
				managedByLabel: managedByValue,
				instanceLabel:  "test-instance",
			},
		},
	}

	// Current config only has the default output, so "stale-svc" should be deleted.
	output := defaultOutput()
	cfgPtr := newTestConfig([]config.OutputConfig{output})

	var applied []appliedService

	var mu sync.Mutex

	var deleted []deletedService

	applyFn := func(
		_ context.Context,
		_ client.WithWatch,
		obj runtime.ApplyConfiguration,
		_ ...client.ApplyOption,
	) error {
		svc, ok := obj.(*applycorev1.ServiceApplyConfiguration)
		if !ok {
			t.Fatalf("Apply called with non-Service object: %T", obj)
		}

		rec := appliedService{}

		if svc.Name != nil {
			rec.name = *svc.Name
		}

		if svc.Namespace != nil {
			rec.namespace = *svc.Namespace
		}

		if svc.Annotations != nil {
			rec.annotations = svc.Annotations
		}

		mu.Lock()
		applied = append(applied, rec)
		mu.Unlock()

		return nil
	}

	deleteFn := func(
		ctx context.Context,
		underlying client.WithWatch,
		obj client.Object,
		opts ...client.DeleteOption,
	) error {
		mu.Lock()
		deleted = append(deleted, deletedService{
			name:      obj.GetName(),
			namespace: obj.GetNamespace(),
		})
		mu.Unlock()

		return underlying.Delete(ctx, obj, opts...)
	}

	rec := buildTestReconcilerWithInterceptor(
		t,
		[]client.Object{eps, staleService},
		cfgPtr,
		interceptor.Funcs{Apply: applyFn, Delete: deleteFn},
	)

	_, err := rec.Reconcile(context.Background(), ctrl.Request{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The current output should have been applied.
	if len(applied) != 1 {
		t.Fatalf("applied %d services, want 1", len(applied))
	}

	if applied[0].name != testSvcDefault {
		t.Errorf("applied service name = %q, want %q", applied[0].name, testSvcDefault)
	}

	// The stale service should have been deleted.
	if len(deleted) != 1 {
		t.Fatalf("deleted %d services, want 1", len(deleted))
	}

	if deleted[0].name != "stale-svc" {
		t.Errorf("deleted service name = %q, want %q", deleted[0].name, "stale-svc")
	}

	if deleted[0].namespace != testSvcNamespace {
		t.Errorf("deleted service namespace = %q, want %q", deleted[0].namespace, testSvcNamespace)
	}
}

func TestReconcile_NoStaleServiceCleanupWhenAllMatch(t *testing.T) {
	eps := makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
		{
			Addresses:  []string{testAddr1},
			Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
		},
	})

	// Pre-existing Service matches the current output config.
	existingService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testSvcDefault,
			Namespace: testSvcNamespace,
			Labels: map[string]string{
				managedByLabel: managedByValue,
				instanceLabel:  "test-instance",
			},
		},
	}

	output := defaultOutput()
	cfgPtr := newTestConfig([]config.OutputConfig{output})

	var deleted []deletedService

	applyFn := func(
		_ context.Context,
		_ client.WithWatch,
		_ runtime.ApplyConfiguration,
		_ ...client.ApplyOption,
	) error {
		return nil
	}

	deleteFn := func(
		ctx context.Context,
		underlying client.WithWatch,
		obj client.Object,
		opts ...client.DeleteOption,
	) error {
		deleted = append(deleted, deletedService{
			name:      obj.GetName(),
			namespace: obj.GetNamespace(),
		})

		return underlying.Delete(ctx, obj, opts...)
	}

	rec := buildTestReconcilerWithInterceptor(
		t,
		[]client.Object{eps, existingService},
		cfgPtr,
		interceptor.Funcs{Apply: applyFn, Delete: deleteFn},
	)

	_, err := rec.Reconcile(context.Background(), ctrl.Request{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// No services should have been deleted since the existing one matches config.
	if len(deleted) != 0 {
		t.Errorf("deleted %d services, want 0 (no stale services)", len(deleted))
	}
}

func TestReconcile_StaleCleanupDeleteError(t *testing.T) {
	eps := makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
		{
			Addresses:  []string{testAddr1},
			Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
		},
	})

	staleService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "stale-svc",
			Namespace: testSvcNamespace,
			Labels: map[string]string{
				managedByLabel: managedByValue,
				instanceLabel:  "test-instance",
			},
		},
	}

	output := defaultOutput()
	cfgPtr := newTestConfig([]config.OutputConfig{output})

	applyFn := func(
		_ context.Context,
		_ client.WithWatch,
		_ runtime.ApplyConfiguration,
		_ ...client.ApplyOption,
	) error {
		return nil
	}

	deleteFn := func(
		_ context.Context,
		_ client.WithWatch,
		_ client.Object,
		_ ...client.DeleteOption,
	) error {
		return errors.New("simulated delete failure")
	}

	rec := buildTestReconcilerWithInterceptor(
		t,
		[]client.Object{eps, staleService},
		cfgPtr,
		interceptor.Funcs{Apply: applyFn, Delete: deleteFn},
	)

	_, err := rec.Reconcile(context.Background(), ctrl.Request{})
	if err == nil {
		t.Fatal("expected error from delete failure, got nil")
	}

	if !strings.Contains(err.Error(), "deleting stale service") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "deleting stale service")
	}
}

func TestReconcile_EmptyOutputsCleansUpAllManagedServices(t *testing.T) {
	eps := makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
		{
			Addresses:  []string{testAddr1},
			Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
		},
	})

	// Two previously-managed Services.
	svc1 := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testSvcA,
			Namespace: testSvcNamespace,
			Labels: map[string]string{
				managedByLabel: managedByValue,
				instanceLabel:  "test-instance",
			},
		},
	}

	svc2 := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testSvcB,
			Namespace: testSvcNamespace,
			Labels: map[string]string{
				managedByLabel: managedByValue,
				instanceLabel:  "test-instance",
			},
		},
	}

	// Config with no outputs -- all managed Services should be deleted.
	cfg := &config.Config{
		Source: config.SourceConfig{
			Namespace:   testNamespace,
			ServiceName: "kubernetes",
		},
		Outputs: []config.OutputConfig{},
	}

	cfgPtr := &atomic.Pointer[config.Config]{}
	cfgPtr.Store(cfg)

	var mu sync.Mutex

	var deleted []deletedService

	applyFn := func(
		_ context.Context,
		_ client.WithWatch,
		_ runtime.ApplyConfiguration,
		_ ...client.ApplyOption,
	) error {
		return nil
	}

	deleteFn := func(
		ctx context.Context,
		underlying client.WithWatch,
		obj client.Object,
		opts ...client.DeleteOption,
	) error {
		mu.Lock()
		deleted = append(deleted, deletedService{
			name:      obj.GetName(),
			namespace: obj.GetNamespace(),
		})
		mu.Unlock()

		return underlying.Delete(ctx, obj, opts...)
	}

	rec := buildTestReconcilerWithInterceptor(
		t,
		[]client.Object{eps, svc1, svc2},
		cfgPtr,
		interceptor.Funcs{Apply: applyFn, Delete: deleteFn},
	)

	_, err := rec.Reconcile(context.Background(), ctrl.Request{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Both stale services should be deleted.
	if len(deleted) != 2 {
		t.Fatalf("deleted %d services, want 2", len(deleted))
	}

	deletedNames := make([]string, 0, len(deleted))
	for _, d := range deleted {
		deletedNames = append(deletedNames, d.name)
	}

	sort.Strings(deletedNames)

	if deletedNames[0] != testSvcA {
		t.Errorf("first deleted service = %q, want %q", deletedNames[0], testSvcA)
	}

	if deletedNames[1] != testSvcB {
		t.Errorf("second deleted service = %q, want %q", deletedNames[1], testSvcB)
	}
}
