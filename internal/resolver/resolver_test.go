package resolver

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func boolPtr(val bool) *bool {
	return &val
}

func strPtr(val string) *string {
	return &val
}

func newScheme(t *testing.T) *runtime.Scheme {
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

func makeEndpointSlice(
	name string,
	addrType discoveryv1.AddressType,
	endpoints []discoveryv1.Endpoint,
) discoveryv1.EndpointSlice {
	return discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: name, Namespace: "default"},
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

func TestResolveAddresses(t *testing.T) {
	tests := []struct {
		name          string
		slices        []discoveryv1.EndpointSlice
		nodes         []*corev1.Node
		addressSource string
		addressType   discoveryv1.AddressType
		want          []string
		wantErr       bool
	}{
		{
			name: "endpointslice source returns IPs directly",
			slices: []discoveryv1.EndpointSlice{
				makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.0.0.1", "10.0.0.2"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
					},
				}),
			},
			addressSource: "endpointslice",
			addressType:   discoveryv1.AddressTypeIPv4,
			want:          []string{"10.0.0.1", "10.0.0.2"},
		},
		{
			name: "endpointslice source filters by address type",
			slices: []discoveryv1.EndpointSlice{
				makeEndpointSlice("eps-v4", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.0.0.1"},
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
			addressSource: "endpointslice",
			addressType:   discoveryv1.AddressTypeIPv6,
			want:          []string{"fd00::1"},
		},
		{
			name: "endpointslice source includes only ready endpoints",
			slices: []discoveryv1.EndpointSlice{
				makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.0.0.1"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
					},
					{
						Addresses:  []string{"10.0.0.2"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(false)},
					},
				}),
			},
			addressSource: "endpointslice",
			addressType:   discoveryv1.AddressTypeIPv4,
			want:          []string{"10.0.0.1"},
		},
		{
			name: "endpointslice source treats nil ready as ready",
			slices: []discoveryv1.EndpointSlice{
				makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.0.0.1"},
						Conditions: discoveryv1.EndpointConditions{Ready: nil},
					},
				}),
			},
			addressSource: "endpointslice",
			addressType:   discoveryv1.AddressTypeIPv4,
			want:          []string{"10.0.0.1"},
		},
		{
			name: "endpointslice source returns sorted deduplicated results",
			slices: []discoveryv1.EndpointSlice{
				makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.0.0.3", "10.0.0.1", "10.0.0.2", "10.0.0.1"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
					},
				}),
			},
			addressSource: "endpointslice",
			addressType:   discoveryv1.AddressTypeIPv4,
			want:          []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"},
		},
		{
			name: "node-internal looks up InternalIP via nodeName",
			slices: []discoveryv1.EndpointSlice{
				makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.244.0.5"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
						NodeName:   strPtr("node-1"),
					},
				}),
			},
			nodes: []*corev1.Node{
				makeNode("node-1", []corev1.NodeAddress{
					{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
					{Type: corev1.NodeExternalIP, Address: "203.0.113.10"},
				}),
			},
			addressSource: "node-internal",
			addressType:   discoveryv1.AddressTypeIPv4,
			want:          []string{"192.168.1.10"},
		},
		{
			name: "node-external looks up ExternalIP via nodeName",
			slices: []discoveryv1.EndpointSlice{
				makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.244.0.5"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
						NodeName:   strPtr("node-1"),
					},
				}),
			},
			nodes: []*corev1.Node{
				makeNode("node-1", []corev1.NodeAddress{
					{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
					{Type: corev1.NodeExternalIP, Address: "203.0.113.10"},
				}),
			},
			addressSource: "node-external",
			addressType:   discoveryv1.AddressTypeIPv4,
			want:          []string{"203.0.113.10"},
		},
		{
			name: "node-public returns non-RFC1918 address",
			slices: []discoveryv1.EndpointSlice{
				makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.244.0.5"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
						NodeName:   strPtr("node-1"),
					},
				}),
			},
			nodes: []*corev1.Node{
				makeNode("node-1", []corev1.NodeAddress{
					{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
					{Type: corev1.NodeExternalIP, Address: "203.0.113.10"},
				}),
			},
			addressSource: "node-public",
			addressType:   discoveryv1.AddressTypeIPv4,
			want:          []string{"203.0.113.10"},
		},
		{
			name: "IP-matching fallback when nodeName is absent",
			slices: []discoveryv1.EndpointSlice{
				makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
					{
						Addresses:  []string{"192.168.1.10"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
					},
				}),
			},
			nodes: []*corev1.Node{
				makeNode("node-1", []corev1.NodeAddress{
					{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
					{Type: corev1.NodeExternalIP, Address: "203.0.113.10"},
				}),
			},
			addressSource: "node-external",
			addressType:   discoveryv1.AddressTypeIPv4,
			want:          []string{"203.0.113.10"},
		},
		{
			name: "node without matching address type is skipped",
			slices: []discoveryv1.EndpointSlice{
				makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.244.0.5"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
						NodeName:   strPtr("node-1"),
					},
				}),
			},
			nodes: []*corev1.Node{
				makeNode("node-1", []corev1.NodeAddress{
					{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
				}),
			},
			addressSource: "node-external",
			addressType:   discoveryv1.AddressTypeIPv4,
			want:          nil,
		},
		{
			name: "multiple EndpointSlices aggregated",
			slices: []discoveryv1.EndpointSlice{
				makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.0.0.1"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
					},
				}),
				makeEndpointSlice("eps-2", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.0.0.2"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
					},
				}),
			},
			addressSource: "endpointslice",
			addressType:   discoveryv1.AddressTypeIPv4,
			want:          []string{"10.0.0.1", "10.0.0.2"},
		},
		{
			name:          "empty EndpointSlice list returns empty",
			slices:        []discoveryv1.EndpointSlice{},
			addressSource: "endpointslice",
			addressType:   discoveryv1.AddressTypeIPv4,
			want:          nil,
		},
		{
			name: "all endpoints not ready returns empty",
			slices: []discoveryv1.EndpointSlice{
				makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.0.0.1"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(false)},
					},
					{
						Addresses:  []string{"10.0.0.2"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(false)},
					},
				}),
			},
			addressSource: "endpointslice",
			addressType:   discoveryv1.AddressTypeIPv4,
			want:          nil,
		},
		{
			name: "endpoint with empty address list",
			slices: []discoveryv1.EndpointSlice{
				makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
					{
						Addresses:  []string{},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
					},
				}),
			},
			addressSource: "endpointslice",
			addressType:   discoveryv1.AddressTypeIPv4,
			want:          nil,
		},
		{
			name: "multiple endpoints on same node are deduplicated",
			slices: []discoveryv1.EndpointSlice{
				makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.244.0.5"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
						NodeName:   strPtr("node-1"),
					},
					{
						Addresses:  []string{"10.244.0.6"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
						NodeName:   strPtr("node-1"),
					},
				}),
			},
			nodes: []*corev1.Node{
				makeNode("node-1", []corev1.NodeAddress{
					{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
				}),
			},
			addressSource: "node-internal",
			addressType:   discoveryv1.AddressTypeIPv4,
			want:          []string{"192.168.1.10"},
		},
		{
			name: "unknown address source returns empty",
			slices: []discoveryv1.EndpointSlice{
				makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.244.0.5"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
						NodeName:   strPtr("node-1"),
					},
				}),
			},
			nodes: []*corev1.Node{
				makeNode("node-1", []corev1.NodeAddress{
					{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
					{Type: corev1.NodeExternalIP, Address: "203.0.113.10"},
				}),
			},
			addressSource: "unknown-source",
			addressType:   discoveryv1.AddressTypeIPv4,
			want:          nil,
		},
		{
			name: "node-public skips hostname and returns empty with only private IP",
			slices: []discoveryv1.EndpointSlice{
				makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.244.0.5"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
						NodeName:   strPtr("node-1"),
					},
				}),
			},
			nodes: []*corev1.Node{
				makeNode("node-1", []corev1.NodeAddress{
					{Type: corev1.NodeHostName, Address: "node.cluster.local"},
					{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
				}),
			},
			addressSource: "node-public",
			addressType:   discoveryv1.AddressTypeIPv4,
			want:          nil,
		},
		{
			name: "node-public with all private IPs returns empty",
			slices: []discoveryv1.EndpointSlice{
				makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.244.0.5"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
						NodeName:   strPtr("node-1"),
					},
				}),
			},
			nodes: []*corev1.Node{
				makeNode("node-1", []corev1.NodeAddress{
					{Type: corev1.NodeInternalIP, Address: "10.0.0.1"},
					{Type: corev1.NodeExternalIP, Address: "192.168.1.10"},
				}),
			},
			addressSource: "node-public",
			addressType:   discoveryv1.AddressTypeIPv4,
			want:          nil,
		},
		{
			name: "node-public with IPv6 ULA returns empty",
			slices: []discoveryv1.EndpointSlice{
				makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.244.0.5"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
						NodeName:   strPtr("node-1"),
					},
				}),
			},
			nodes: []*corev1.Node{
				makeNode("node-1", []corev1.NodeAddress{
					{Type: corev1.NodeInternalIP, Address: "fc00::1"},
				}),
			},
			addressSource: "node-public",
			addressType:   discoveryv1.AddressTypeIPv4,
			want:          nil,
		},
		{
			name: "IP-matching fallback with unmatched IP warns and returns empty",
			slices: []discoveryv1.EndpointSlice{
				makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.99.99.99"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
					},
				}),
			},
			nodes: []*corev1.Node{
				makeNode("node-1", []corev1.NodeAddress{
					{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
				}),
			},
			addressSource: "node-internal",
			addressType:   discoveryv1.AddressTypeIPv4,
			want:          nil,
		},
		{
			name: "mixed nodeName and IP-matching resolution",
			slices: []discoveryv1.EndpointSlice{
				makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.244.0.5"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
						NodeName:   strPtr("node-1"),
					},
					{
						Addresses:  []string{"192.168.1.20"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
					},
				}),
			},
			nodes: []*corev1.Node{
				makeNode("node-1", []corev1.NodeAddress{
					{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
				}),
				makeNode("node-2", []corev1.NodeAddress{
					{Type: corev1.NodeInternalIP, Address: "192.168.1.20"},
				}),
			},
			addressSource: "node-internal",
			addressType:   discoveryv1.AddressTypeIPv4,
			want:          []string{"192.168.1.10", "192.168.1.20"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := newScheme(t)
			builder := fake.NewClientBuilder().WithScheme(scheme)

			objs := make([]runtime.Object, 0, len(tt.nodes))
			for _, node := range tt.nodes {
				objs = append(objs, node)
			}

			cli := builder.WithRuntimeObjects(objs...).Build()
			res := NewResolver(cli, slog.New(slog.NewTextHandler(os.Stderr, nil)))

			got, err := res.ResolveAddresses(
				context.Background(),
				tt.slices,
				tt.addressSource,
				tt.addressType,
			)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if !equalStringSlices(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func equalStringSlices(got, want []string) bool {
	if len(got) == 0 && len(want) == 0 {
		return true
	}

	if len(got) != len(want) {
		return false
	}

	for idx := range got {
		if got[idx] != want[idx] {
			return false
		}
	}

	return true
}

var (
	errNodeGet  = errors.New("simulated node get failure")
	errNodeList = errors.New("simulated node list failure")
)

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}

func TestResolveAddresses_NodeGetError(t *testing.T) {
	scheme := newScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(
			makeNode("node-1", []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
			}),
		).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(
				ctx context.Context,
				cli client.WithWatch,
				key client.ObjectKey,
				obj client.Object,
				opts ...client.GetOption,
			) error {
				if _, isNode := obj.(*corev1.Node); isNode {
					return errNodeGet
				}

				return cli.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	res := NewResolver(fakeClient, newTestLogger())

	slices := []discoveryv1.EndpointSlice{
		makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
			{
				Addresses:  []string{"10.244.0.5"},
				Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
				NodeName:   strPtr("node-1"),
			},
		}),
	}

	got, err := res.ResolveAddresses(
		context.Background(), slices, "node-internal", discoveryv1.AddressTypeIPv4,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Node Get fails, so the node is skipped and result is empty.
	if !equalStringSlices(got, nil) {
		t.Errorf("got %v, want nil", got)
	}
}

func TestResolveAddresses_NodeListError(t *testing.T) {
	scheme := newScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(
				ctx context.Context,
				cli client.WithWatch,
				list client.ObjectList,
				opts ...client.ListOption,
			) error {
				if _, isNodeList := list.(*corev1.NodeList); isNodeList {
					return errNodeList
				}

				return cli.List(ctx, list, opts...)
			},
		}).
		Build()

	res := NewResolver(fakeClient, newTestLogger())

	// Endpoints without nodeName trigger IP-matching fallback which calls List.
	slices := []discoveryv1.EndpointSlice{
		makeEndpointSlice("eps-1", discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
			{
				Addresses:  []string{"192.168.1.10"},
				Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
			},
		}),
	}

	_, err := res.ResolveAddresses(
		context.Background(), slices, "node-internal", discoveryv1.AddressTypeIPv4,
	)
	if err == nil {
		t.Fatal("expected error from node list failure, got nil")
	}
}

func TestSortedUnique(t *testing.T) {
	tests := []struct {
		name  string
		input []string
		want  []string
	}{
		{
			name:  "empty input returns empty",
			input: []string{},
			want:  []string{},
		},
		{
			name:  "nil input returns nil",
			input: nil,
			want:  nil,
		},
		{
			name:  "all duplicates returns single item",
			input: []string{"aaa", "aaa", "aaa"},
			want:  []string{"aaa"},
		},
		{
			name:  "already sorted unique is unchanged",
			input: []string{"a", "b", "c"},
			want:  []string{"a", "b", "c"},
		},
		{
			name:  "unsorted with duplicates",
			input: []string{"c", "a", "b", "a", "c"},
			want:  []string{"a", "b", "c"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sortedUnique(tt.input)
			if !equalStringSlices(got, tt.want) {
				t.Errorf("sortedUnique(%v) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestFindAddressByType(t *testing.T) {
	addresses := []corev1.NodeAddress{
		{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
		{Type: corev1.NodeExternalIP, Address: "203.0.113.10"},
	}

	got := findAddressByType(addresses, corev1.NodeHostName)
	if got != "" {
		t.Errorf("findAddressByType with no matching type = %q, want empty", got)
	}
}

func TestFindNodeAddress_UnknownSource(t *testing.T) {
	node := makeNode("test-node", []corev1.NodeAddress{
		{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
		{Type: corev1.NodeExternalIP, Address: "203.0.113.10"},
	})

	got := findNodeAddress(node, "unknown-source")
	if got != "" {
		t.Errorf("findNodeAddress with unknown source = %q, want empty", got)
	}
}

func TestFindPublicAddress(t *testing.T) {
	tests := []struct {
		name      string
		addresses []corev1.NodeAddress
		want      string
	}{
		{
			name: "skips hostname and returns empty with only private IP",
			addresses: []corev1.NodeAddress{
				{Type: corev1.NodeHostName, Address: "node.cluster.local"},
				{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
			},
			want: "",
		},
		{
			name: "all private IPs returns empty",
			addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "10.0.0.1"},
				{Type: corev1.NodeExternalIP, Address: "192.168.1.10"},
				{Type: corev1.NodeInternalIP, Address: "172.16.0.1"},
			},
			want: "",
		},
		{
			name: "IPv6 ULA returns empty",
			addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "fc00::1"},
			},
			want: "",
		},
		{
			name: "returns first public IP",
			addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "10.0.0.1"},
				{Type: corev1.NodeExternalIP, Address: "203.0.113.10"},
			},
			want: "203.0.113.10",
		},
		{
			name:      "empty addresses returns empty",
			addresses: nil,
			want:      "",
		},
		{
			name: "hostname only returns empty",
			addresses: []corev1.NodeAddress{
				{Type: corev1.NodeHostName, Address: "my-node.example.com"},
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findPublicAddress(tt.addresses)
			if got != tt.want {
				t.Errorf("findPublicAddress() = %q, want %q", got, tt.want)
			}
		})
	}
}
