package k0s

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"sync"

	dockernetwork "github.com/docker/docker/api/types/network"
	clustersv1alpha1 "github.com/openmcp-project/openmcp-operator/api/clusters/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openmcp-project/cluster-provider-k0s/api/v1alpha1"
)

// Subnet slices of the docker network handed to clusters as metallb pools;
// the third octet range keeps them clear of docker's own allocations.
const (
	subnetMin = 200
	subnetMax = 255
)

var (
	errIPv4NetworkNotFound = errors.New("ipv4 network not found")
	errUnsupportedNetwork  = errors.New("unsupported network. Subnet mask should be either 8 or 16 out of 32")
	errNoSubnetsAvailable  = errors.New("no subnets available")

	// AnnotationAssignedSubnet stores the metallb subnet assigned to a Cluster.
	AnnotationAssignedSubnet = v1alpha1.GroupVersion.Group + "/assigned-subnet"

	// lockListClusters serializes subnet allocation so concurrent reconciles
	// cannot hand out the same slice.
	lockListClusters = sync.Mutex{}
)

// NextAvailableLBNetwork implements Provider.
func (provider *k0sProvider) NextAvailableLBNetwork(ctx context.Context, c client.Client) (net.IPNet, error) {
	lockListClusters.Lock()
	defer lockListClusters.Unlock()

	dockerNetwork, err := provider.networkSubnet(ctx)
	if err != nil {
		return net.IPNet{}, err
	}

	clusters := &clustersv1alpha1.ClusterList{}
	if err := c.List(ctx, clusters); err != nil {
		return net.IPNet{}, fmt.Errorf("listing Clusters: %w", err)
	}

	for i := subnetMin; i <= subnetMax; i++ {
		subnet, err := calculateV4Subnet(dockerNetwork, i)
		if err != nil {
			return net.IPNet{}, err
		}

		taken, err := isIPNetTaken(subnet, clusters)
		if err != nil {
			return net.IPNet{}, err
		}
		if taken {
			continue
		}
		return subnet, nil
	}
	return net.IPNet{}, errNoSubnetsAvailable
}

// networkSubnet returns the IPv4 subnet of the configured docker dockernetwork.
func (provider *k0sProvider) networkSubnet(ctx context.Context) (net.IPNet, error) {
	inspect, err := provider.docker.NetworkInspect(ctx, provider.opts.Network, dockernetwork.InspectOptions{})
	if err != nil {
		return net.IPNet{}, fmt.Errorf("inspecting docker network %q: %w", provider.opts.Network, err)
	}
	for _, cfg := range inspect.IPAM.Config {
		_, parsedNet, err := net.ParseCIDR(cfg.Subnet)
		if err != nil {
			return net.IPNet{}, fmt.Errorf("parsing subnet %q of docker network %q: %w", cfg.Subnet, provider.opts.Network, err)
		}
		if isIPv4(parsedNet) {
			return *parsedNet, nil
		}
	}
	return net.IPNet{}, errIPv4NetworkNotFound
}

// calculateV4Subnet returns a /24 subnet of the given net.IPNet. Must be a /8 or /16 dockernetwork.
func calculateV4Subnet(input net.IPNet, offset int) (net.IPNet, error) {
	inputV4 := input.IP.To4()
	ones, bits := input.Mask.Size()

	if (ones != 8 && ones != 16) || bits != 32 {
		return net.IPNet{}, errUnsupportedNetwork
	}

	subnetIP := slices.Clone(inputV4)
	subnetIP[2] = byte(offset)

	return net.IPNet{
		IP:   subnetIP,
		Mask: net.CIDRMask(24, 32),
	}, nil
}

func isIPNetTaken(ipnet net.IPNet, clusters *clustersv1alpha1.ClusterList) (bool, error) {
	for _, c := range clusters.Items {
		cNet, err := SubnetFromCluster(&c)
		if err != nil {
			return false, err
		}
		if cNet == nil {
			continue
		}
		if cNet.IP.Equal(ipnet.IP) {
			return true, nil
		}
	}
	return false, nil
}

// SubnetFromCluster extracts the assigned subnet from the cluster annotations.
func SubnetFromCluster(c *clustersv1alpha1.Cluster) (*net.IPNet, error) {
	ipNetStr, ok := c.Annotations[AnnotationAssignedSubnet]
	if !ok {
		return nil, nil
	}

	_, parsedNet, err := net.ParseCIDR(ipNetStr)
	if err != nil {
		return nil, fmt.Errorf("parsing assigned subnet %q: %w", ipNetStr, err)
	}

	return parsedNet, nil
}

func isIPv4(ipNet *net.IPNet) bool {
	return ipNet.IP.To4() != nil
}

// ensureNetwork creates the configured docker network if it does not exist.
func (provider *k0sProvider) ensureNetwork(ctx context.Context) error {
	if _, err := provider.docker.NetworkInspect(ctx, provider.opts.Network, dockernetwork.InspectOptions{}); err == nil {
		return nil
	}

	_, err := provider.docker.NetworkCreate(ctx, provider.opts.Network, dockernetwork.CreateOptions{})
	if err != nil {
		return fmt.Errorf("creating docker network %q: %w", provider.opts.Network, err)
	}

	return nil
}
