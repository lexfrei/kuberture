package resolver

import (
	"context"
	"log/slog"
	"net/netip"
	"slices"
	"sort"

	"github.com/cockroachdb/errors"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lexfrei/kuberture/internal/config"
)

// Resolver resolves node addresses from EndpointSlice resources.
type Resolver struct {
	client client.Client
	log    *slog.Logger
}

// NewResolver creates a new Resolver with the given controller-runtime client.
func NewResolver(cli client.Client, log *slog.Logger) *Resolver {
	return &Resolver{client: cli, log: log}
}

// endpointInfo holds an endpoint address and its optional node name.
type endpointInfo struct {
	address  string
	nodeName string
}

// ResolveAddresses resolves endpoint addresses based on the given source and type.
func (res *Resolver) ResolveAddresses(
	ctx context.Context,
	endpointSlices []discoveryv1.EndpointSlice,
	addressSource string,
	addressType discoveryv1.AddressType,
) ([]string, error) {
	infos := collectReadyAddresses(endpointSlices, addressType)

	if addressSource == config.AddressSourceEndpointSlice {
		return sortedUnique(extractAddresses(infos)), nil
	}

	addresses, err := res.resolveNodeAddresses(ctx, infos, addressSource)
	if err != nil {
		return nil, errors.Wrap(err, "resolving node addresses")
	}

	return sortedUnique(addresses), nil
}

// collectReadyAddresses extracts addresses and node names from ready endpoints.
func collectReadyAddresses(
	endpointSlices []discoveryv1.EndpointSlice,
	addressType discoveryv1.AddressType,
) []endpointInfo {
	var infos []endpointInfo

	for idx := range endpointSlices {
		eps := &endpointSlices[idx]
		if eps.AddressType != addressType {
			continue
		}

		for epIdx := range eps.Endpoints {
			endpoint := &eps.Endpoints[epIdx]
			if !isEndpointReady(endpoint) {
				continue
			}

			nodeName := ""
			if endpoint.NodeName != nil {
				nodeName = *endpoint.NodeName
			}

			for _, addr := range endpoint.Addresses {
				infos = append(infos, endpointInfo{
					address:  addr,
					nodeName: nodeName,
				})
			}
		}
	}

	return infos
}

// resolveNodeAddresses resolves addresses by looking up node objects.
func (res *Resolver) resolveNodeAddresses(
	ctx context.Context,
	infos []endpointInfo,
	addressSource string,
) ([]string, error) {
	nodeNames, err := res.buildNodeNameSet(ctx, infos)
	if err != nil {
		return nil, err
	}

	var addresses []string

	for nodeName := range nodeNames {
		addr, err := res.getNodeAddress(ctx, nodeName, addressSource)
		if err != nil {
			res.log.Warn("skipping node: address not found",
				slog.String("node", nodeName),
				slog.String("source", addressSource),
				slog.String("error", err.Error()),
			)

			continue
		}

		addresses = append(addresses, addr)
	}

	return addresses, nil
}

// buildNodeNameSet builds a set of node names from endpoint infos.
func (res *Resolver) buildNodeNameSet(
	ctx context.Context,
	infos []endpointInfo,
) (map[string]struct{}, error) {
	nodeNames := make(map[string]struct{})

	var unmatchedIPs []string

	for _, info := range infos {
		if info.nodeName != "" {
			nodeNames[info.nodeName] = struct{}{}
		} else {
			unmatchedIPs = append(unmatchedIPs, info.address)
		}
	}

	if len(unmatchedIPs) == 0 {
		return nodeNames, nil
	}

	matched, err := res.matchIPsToNodes(ctx, unmatchedIPs)
	if err != nil {
		return nil, err
	}

	for name := range matched {
		nodeNames[name] = struct{}{}
	}

	return nodeNames, nil
}

// matchIPsToNodes lists all nodes and matches endpoint IPs to node names.
func (res *Resolver) matchIPsToNodes(
	ctx context.Context,
	ips []string,
) (map[string]struct{}, error) {
	var nodeList corev1.NodeList

	err := res.client.List(ctx, &nodeList)
	if err != nil {
		return nil, errors.Wrap(err, "listing nodes")
	}

	ipToNode := buildIPToNodeMap(nodeList.Items)
	result := make(map[string]struct{})

	for _, addr := range ips {
		if name, found := ipToNode[addr]; found {
			result[name] = struct{}{}
		} else {
			res.log.Warn("no node found for endpoint IP", slog.String("ip", addr))
		}
	}

	return result, nil
}

// buildIPToNodeMap creates a mapping from node IP addresses to node names.
func buildIPToNodeMap(nodes []corev1.Node) map[string]string {
	ipToNode := make(map[string]string)

	for idx := range nodes {
		for _, addr := range nodes[idx].Status.Addresses {
			ipToNode[addr.Address] = nodes[idx].Name
		}
	}

	return ipToNode
}

// getNodeAddress fetches a node and extracts the desired address.
func (res *Resolver) getNodeAddress(
	ctx context.Context,
	nodeName string,
	addressSource string,
) (string, error) {
	var node corev1.Node

	key := types.NamespacedName{Name: nodeName}

	err := res.client.Get(ctx, key, &node)
	if err != nil {
		return "", errors.Wrapf(err, "getting node %s", nodeName)
	}

	addr := findNodeAddress(&node, addressSource)
	if addr == "" {
		return "", errors.Newf("no %s address found on node %s", addressSource, nodeName)
	}

	return addr, nil
}

// findNodeAddress extracts a single address from a node based on the source.
func findNodeAddress(node *corev1.Node, addressSource string) string {
	switch addressSource {
	case config.AddressSourceNodeInternal:
		return findAddressByType(node.Status.Addresses, corev1.NodeInternalIP)
	case config.AddressSourceNodeExternal:
		return findAddressByType(node.Status.Addresses, corev1.NodeExternalIP)
	case config.AddressSourceNodePublic:
		return findPublicAddress(node.Status.Addresses)
	default:
		return ""
	}
}

// findAddressByType returns the first address matching the given type.
func findAddressByType(addresses []corev1.NodeAddress, addrType corev1.NodeAddressType) string {
	for _, addr := range addresses {
		if addr.Type == addrType {
			return addr.Address
		}
	}

	return ""
}

// findPublicAddress returns the first non-private IP address from the list.
// Addresses that cannot be parsed as IPs (e.g. hostnames) are skipped.
func findPublicAddress(addresses []corev1.NodeAddress) string {
	for _, addr := range addresses {
		parsed, parseErr := netip.ParseAddr(addr.Address)
		if parseErr != nil {
			continue
		}

		if !isPrivateAddr(parsed) {
			return addr.Address
		}
	}

	return ""
}

// extractAddresses pulls just the address strings from endpoint infos.
func extractAddresses(infos []endpointInfo) []string {
	addresses := make([]string, 0, len(infos))

	for _, info := range infos {
		addresses = append(addresses, info.address)
	}

	return addresses
}

// sortedUnique returns a sorted slice with duplicates removed.
func sortedUnique(items []string) []string {
	sort.Strings(items)

	return slices.Compact(items)
}

// isEndpointReady reports whether an endpoint should be considered ready.
func isEndpointReady(endpoint *discoveryv1.Endpoint) bool {
	return endpoint.Conditions.Ready == nil || *endpoint.Conditions.Ready
}
