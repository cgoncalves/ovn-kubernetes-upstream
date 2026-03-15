package ovn

import (
	"fmt"
	"net"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	networkmanager "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	// EgressGatewayReroute is the policy name for the egress gateway reroute policy.
	EgressGatewayReroute = "EgressGW-Reroute"
)

// EgressGatewayController manages the egress gateway reroute policies that steer
// pod egress traffic through egress-assignable nodes.
type EgressGatewayController struct {
	nbClient          libovsdbclient.Client
	watchFactory      *factory.WatchFactory
	addressSetFactory addressset.AddressSetFactory
	networkManager    networkmanager.Interface
	controllerName    string
	zone              string
	v4, v6            bool
	// mu serializes policy updates
	mu sync.Mutex
}

// NewEgressGatewayController creates a new EgressGatewayController.
func NewEgressGatewayController(
	nbClient libovsdbclient.Client,
	watchFactory *factory.WatchFactory,
	addressSetFactory addressset.AddressSetFactory,
	networkManager networkmanager.Interface,
	controllerName string,
	zone string,
	v4, v6 bool,
) *EgressGatewayController {
	return &EgressGatewayController{
		nbClient:          nbClient,
		watchFactory:      watchFactory,
		addressSetFactory: addressSetFactory,
		networkManager:    networkManager,
		controllerName:    controllerName,
		zone:              zone,
		v4:                v4,
		v6:                v6,
	}
}

// getEgressGatewayLRPDbIDs returns the DB IDs for the egress gateway reroute logical router policy.
func getEgressGatewayLRPDbIDs(ipFamily egressIPFamilyValue, network, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.LogicalRouterPolicyEgressIP, controller, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey: EgressGatewayReroute,
		libovsdbops.PriorityKey:   fmt.Sprintf("%d", types.EgressGatewayReroutePriority),
		libovsdbops.IPFamilyKey:   string(ipFamily),
		libovsdbops.NetworkKey:    network,
	})
}

// isLocalZoneNode checks whether the given node belongs to the local zone.
func (e *EgressGatewayController) isLocalZoneNode(node *corev1.Node) bool {
	return util.GetNodeZone(node) == e.zone
}

// getGatewayRouterNextHop retrieves the gateway router join port IP for the given node.
// This is used to determine the next hop for local-zone egress nodes.
func (e *EgressGatewayController) getGatewayRouterNextHop(ni util.NetInfo, node *corev1.Node, isIPv6 bool) (net.IP, error) {
	portName := types.GWRouterToJoinSwitchPrefix + ni.GetNetworkScopedGWRouterName(node.Name)
	gatewayIPs, err := libovsdbutil.GetLRPAddrs(e.nbClient, portName)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve port %s IP(s): %w", portName, err)
	}
	gatewayIP, err := util.MatchFirstIPNetFamily(isIPv6, gatewayIPs)
	if err != nil {
		return nil, fmt.Errorf("failed to find IP for port %s for IP family %v: %v", portName, isIPv6, err)
	}
	return gatewayIP.IP, nil
}

// getTransitIP retrieves the transit switch IP for the given node name.
// This is used to determine the next hop for remote-zone egress nodes in IC mode.
func (e *EgressGatewayController) getTransitIP(nodeName string, wantsIPv6 bool) (string, error) {
	node, err := e.watchFactory.GetNode(nodeName)
	if err != nil {
		return "", fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}
	nodeTransitIPs, err := util.ParseNodeTransitSwitchPortAddrs(node)
	if err != nil {
		return "", fmt.Errorf("unable to fetch transit switch IP for node %s: %w", nodeName, err)
	}
	nodeTransitIP, err := util.MatchFirstIPNetFamily(wantsIPv6, nodeTransitIPs)
	if err != nil {
		return "", fmt.Errorf("could not find transit switch IP of node %v for this family %v: %v", node, wantsIPv6, err)
	}
	return nodeTransitIP.IP.String(), nil
}

// ensureReroutePolicy ensures that a low-priority reroute policy exists on the cluster
// router that routes pod egress traffic through egress-assignable nodes. When source CIDRs are configured
// via egress-gateway-source-cidrs, only traffic from those CIDRs is steered. Otherwise, traffic from
// all cluster pod subnets is steered (catch-all).
//
// The policy is created at priority EgressGatewayReroutePriority with match
// `ip4.src == <cidr> && ip4.dst != $<node-ips> && ip4.dst != 169.254.0.0/16` and action reroute
// with next hops pointing to the gateway router IPs of all egress-assignable nodes.
// East-west traffic (pod-to-pod, pod-to-service) is excluded by higher-priority no-reroute policies.
// Pod-to-node and link-local traffic is excluded by the match expression itself.
// When no egress-assignable nodes exist, the policy is removed.
func (e *EgressGatewayController) ensureReroutePolicy() error {
	if !config.OVNKubernetesFeature.EnableEgressGateway {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	ni := e.networkManager.GetNetwork(types.DefaultNetworkName)
	routerName := ni.GetNetworkScopedClusterRouterName()

	// Get all nodes
	nodes, err := e.watchFactory.GetNodes()
	if err != nil {
		return fmt.Errorf("failed to list nodes: %v", err)
	}

	// Collect next hop IPs for egress-assignable nodes.
	// For local-zone egress nodes: use the gateway router join port IP.
	// For remote-zone egress nodes (IC mode): use the transit switch IP.
	var v4NextHops, v6NextHops []string
	egressLabel := util.GetNodeEgressLabel()
	for _, node := range nodes {
		if _, ok := node.Labels[egressLabel]; !ok {
			continue
		}
		isLocal := e.isLocalZoneNode(node)
		if e.v4 {
			var nextHop string
			if isLocal {
				gwIP, err := e.getGatewayRouterNextHop(ni, node, false)
				if err != nil {
					klog.Warningf("Failed to get IPv4 gateway next hop for egress node %s: %v", node.Name, err)
					continue
				}
				nextHop = gwIP.String()
			} else if config.OVNKubernetesFeature.EnableInterconnect {
				transitIP, err := e.getTransitIP(node.Name, false)
				if err != nil {
					klog.Warningf("Failed to get IPv4 transit switch IP for remote egress node %s: %v", node.Name, err)
					continue
				}
				nextHop = transitIP
			} else {
				continue
			}
			v4NextHops = append(v4NextHops, nextHop)
		}
		if e.v6 {
			var nextHop string
			if isLocal {
				gwIP, err := e.getGatewayRouterNextHop(ni, node, true)
				if err != nil {
					klog.Warningf("Failed to get IPv6 gateway next hop for egress node %s: %v", node.Name, err)
					continue
				}
				nextHop = gwIP.String()
			} else if config.OVNKubernetesFeature.EnableInterconnect {
				transitIP, err := e.getTransitIP(node.Name, true)
				if err != nil {
					klog.Warningf("Failed to get IPv6 transit switch IP for remote egress node %s: %v", node.Name, err)
					continue
				}
				nextHop = transitIP
			} else {
				continue
			}
			v6NextHops = append(v6NextHops, nextHop)
		}
	}

	// Determine source CIDRs: use configured CIDRs if set, otherwise fall back to cluster pod subnets
	var subnets []*net.IPNet
	if len(config.OVNKubernetesFeature.EgressGatewaySourceCIDRs) > 0 {
		subnets = config.OVNKubernetesFeature.EgressGatewaySourceCIDRs
	} else {
		subnetEntries := ni.Subnets()
		subnets = util.GetAllClusterSubnetsFromEntries(subnetEntries)
	}

	// Get node IP address set hash names for pod-to-node exclusion in the match expression
	var v4NodeIPAS, v6NodeIPAS string
	dbIDs := getEgressIPAddrSetDbIDs(NodeIPAddrSetName, types.DefaultNetworkName, types.DefaultNetworkControllerName)
	as, err := e.addressSetFactory.GetAddressSet(dbIDs)
	if err != nil {
		klog.Warningf("Failed to get node IP address set for egress gateway match exclusion: %v", err)
	} else {
		v4NodeIPAS, v6NodeIPAS = as.GetASHashNames()
	}

	// Phase 1: Delete all existing egress gateway reroute policies
	var deleteOps []ovsdb.Operation
	if e.v4 {
		dbIDs := getEgressGatewayLRPDbIDs(IPFamilyValueV4, ni.GetNetworkName(), e.controllerName)
		p := libovsdbops.GetPredicate[*nbdb.LogicalRouterPolicy](dbIDs, nil)
		deleteOps, err = libovsdbops.DeleteLogicalRouterPolicyWithPredicateOps(e.nbClient, deleteOps, routerName, p)
		if err != nil {
			return fmt.Errorf("failed to delete existing IPv4 egress gateway reroute policy: %v", err)
		}
	}
	if e.v6 {
		dbIDs := getEgressGatewayLRPDbIDs(IPFamilyValueV6, ni.GetNetworkName(), e.controllerName)
		p := libovsdbops.GetPredicate[*nbdb.LogicalRouterPolicy](dbIDs, nil)
		deleteOps, err = libovsdbops.DeleteLogicalRouterPolicyWithPredicateOps(e.nbClient, deleteOps, routerName, p)
		if err != nil {
			return fmt.Errorf("failed to delete existing IPv6 egress gateway reroute policy: %v", err)
		}
	}
	if len(deleteOps) > 0 {
		if _, err := libovsdbops.TransactAndCheck(e.nbClient, deleteOps); err != nil {
			return fmt.Errorf("failed to delete egress gateway reroute policies: %v", err)
		}
	}

	// Phase 2: Create new policies if egress-assignable nodes exist
	if e.v4 && len(v4NextHops) > 0 {
		for _, subnet := range subnets {
			if utilnet.IsIPv6CIDR(subnet) {
				continue
			}
			match := fmt.Sprintf("ip4.src == %s", subnet.String())
			// Exclude pod-to-node traffic (node IPs are in the address set)
			if v4NodeIPAS != "" {
				match += fmt.Sprintf(" && ip4.dst != $%s", v4NodeIPAS)
			}
			// Exclude link-local traffic
			match += " && ip4.dst != 169.254.0.0/16"
			dbIDs := getEgressGatewayLRPDbIDs(IPFamilyValueV4, ni.GetNetworkName(), e.controllerName)
			lrp := nbdb.LogicalRouterPolicy{
				Match:       match,
				Priority:    types.EgressGatewayReroutePriority,
				Nexthops:    v4NextHops,
				Action:      nbdb.LogicalRouterPolicyActionReroute,
				ExternalIDs: dbIDs.GetExternalIDs(),
			}
			p := libovsdbops.GetPredicate[*nbdb.LogicalRouterPolicy](dbIDs, func(item *nbdb.LogicalRouterPolicy) bool {
				return item.Match == match
			})
			if err := libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicate(e.nbClient, routerName, &lrp, p); err != nil {
				return fmt.Errorf("failed to create IPv4 egress gateway reroute policy for subnet %s: %v", subnet.String(), err)
			}
		}
	}

	if e.v6 && len(v6NextHops) > 0 {
		for _, subnet := range subnets {
			if !utilnet.IsIPv6CIDR(subnet) {
				continue
			}
			match := fmt.Sprintf("ip6.src == %s", subnet.String())
			// Exclude pod-to-node traffic (node IPs are in the address set)
			if v6NodeIPAS != "" {
				match += fmt.Sprintf(" && ip6.dst != $%s", v6NodeIPAS)
			}
			// Exclude link-local traffic
			match += " && ip6.dst != fe80::/10"
			dbIDs := getEgressGatewayLRPDbIDs(IPFamilyValueV6, ni.GetNetworkName(), e.controllerName)
			lrp := nbdb.LogicalRouterPolicy{
				Match:       match,
				Priority:    types.EgressGatewayReroutePriority,
				Nexthops:    v6NextHops,
				Action:      nbdb.LogicalRouterPolicyActionReroute,
				ExternalIDs: dbIDs.GetExternalIDs(),
			}
			p := libovsdbops.GetPredicate[*nbdb.LogicalRouterPolicy](dbIDs, func(item *nbdb.LogicalRouterPolicy) bool {
				return item.Match == match
			})
			if err := libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicate(e.nbClient, routerName, &lrp, p); err != nil {
				return fmt.Errorf("failed to create IPv6 egress gateway reroute policy for subnet %s: %v", subnet.String(), err)
			}
		}
	}

	klog.V(4).Infof("Egress gateway reroute policy ensured with %d IPv4 and %d IPv6 next hops", len(v4NextHops), len(v6NextHops))
	return nil
}
