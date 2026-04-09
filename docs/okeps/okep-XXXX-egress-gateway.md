# OKEP-XXXX: Egress Gateway

* Issue: [#XXXX](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/XXXX)

## Problem Statement

In a multi-node Kubernetes cluster, pod egress traffic exits via whatever node the pod is running on, making the egress point unpredictable. Cluster administrators need a way to ensure all pod egress traffic exits through a controlled set of designated nodes so that external firewalls, audit systems, and network policies can rely on a known, stable set of egress IPs.

## Goals

* Provide a mechanism to route **all** pod egress traffic through designated egress nodes labeled with `k8s.ovn.org/egress-assignable`.
* Traffic exiting via egress nodes is SNATed with the egress node's own IP address (default masquerade), providing predictable source IPs.
* Support ECMP-based load distribution across multiple egress-assignable nodes.
* Allow administrators to optionally restrict the feature to specific source CIDRs instead of all pod subnets.
* Maintain full compatibility with EgressIP, EgressService, and other existing egress features through a clear priority hierarchy.
* Support both single-zone and multi-zone (Interconnect) deployments.
* Support both IPv4 and IPv6 (including dual-stack).

## Non-Goals

* Per-pod or per-namespace egress IP assignment (handled by the EgressIP feature).
* SNAT to a specific IP address: the Egress Gateway always uses the egress node's own IP via default masquerade.
* Steering east-west traffic (pod-to-pod, pod-to-service, pod-to-node) through egress nodes.
* Introducing any new CRDs or API objects; the feature is controlled entirely through configuration flags and node labels.

## Introduction

Kubernetes clusters often need to control the egress point for external traffic. External systems such as firewalls, intrusion detection systems, and partner APIs frequently rely on allowlisting specific source IPs. When pods are spread across many nodes, each node's IP becomes a potential egress source, making it difficult to maintain these allowlists.

The Egress Gateway feature addresses this by creating a low-priority catch-all reroute policy on the OVN cluster router that steers all pod egress traffic to nodes labeled as egress-assignable. On those nodes, traffic undergoes default masquerade (SNAT to the node IP), ensuring a predictable and small set of egress source addresses.

This feature is independent from EgressIP. While EgressIP provides fine-grained per-pod SNAT control for specific pods, the Egress Gateway provides a cluster-wide catch-all for all remaining pods. When both features are enabled, they coexist through a clear priority hierarchy where EgressIP takes precedence.

## User-Stories/Use-Cases

### Story 1: Predictable egress for firewall allowlisting

**As a** cluster administrator,
**I want** all pod egress traffic to exit through a known set of nodes,
**so that** I can configure external firewalls to allowlist only those nodes' IPs rather than every node in the cluster.

### Story 2: Centralized egress auditing

**As a** security engineer,
**I want** all external-bound traffic to pass through designated egress nodes,
**so that** I can deploy network monitoring and auditing tools on those nodes to inspect all outbound traffic.

### Story 3: Selective source CIDR steering

**As a** cluster administrator with multiple pod subnets,
**I want** to steer only specific pod subnets through egress nodes,
**so that** I can apply egress controls to sensitive workloads while leaving other subnets unaffected.

### Story 4: Coexistence with EgressIP

**As a** cluster administrator,
**I want** to use both EgressIP for specific high-priority pods and the Egress Gateway for everything else,
**so that** critical pods get dedicated egress IPs while all other pods still exit through controlled egress nodes.

## Proposed Solution

### API Details

The Egress Gateway does not introduce any new CRDs or Kubernetes API objects. It is controlled through:

1. **Configuration flags** on the ovnkube binary:
   - `--enable-egress-gateway` (boolean, default `false`): Enables the feature.
   - `--egress-gateway-source-cidrs` (comma-separated CIDRs, default empty): When set, only pods with IPs matching the specified CIDRs have their egress traffic steered. When empty, all cluster pod subnets are steered (catch-all). Both IPv4 and IPv6 CIDRs are supported. This flag is only valid when `--enable-egress-gateway` is `true`.

   These can also be set via the config file:
   ```
   [ovnkubernetesfeature]
   enable-egress-gateway=true
   egress-gateway-source-cidrs=10.132.0.0/14,10.136.0.0/14
   ```

2. **Node labels**: Nodes must be labeled with `k8s.ovn.org/egress-assignable` to be selected as egress gateway nodes:
   ```shell
   kubectl label nodes <node_name> k8s.ovn.org/egress-assignable=""
   ```

### Implementation Details

#### EgressGatewayController

The core logic is implemented in the `EgressGatewayController` (defined in `go-controller/pkg/ovn/egress_gateway.go`). This controller manages the lifecycle of the egress gateway reroute policy on the OVN cluster router (`ovn_cluster_router`).

The controller is initialized when the `--enable-egress-gateway` flag is set to `true`. It watches for node label changes and re-evaluates the reroute policy whenever the set of egress-assignable nodes changes.

#### Logical Router Policy

The controller creates a logical router policy on `ovn_cluster_router` at **priority 98** (`EgressGatewayReroutePriority`) with:

- **Match**: `ip4.src == <pod-subnet> && ip4.dst != $<node-ips-address-set> && ip4.dst != 169.254.0.0/16`
  - For IPv6: `ip6.src == <pod-subnet> && ip6.dst != $<node-ips-address-set> && ip6.dst != fe80::/10`
- **Action**: `reroute`
- **Next hops**: Gateway router join port IPs of all egress-assignable nodes (ECMP)

The match expression ensures:
- Only pod-originated traffic is matched (via source CIDR).
- Pod-to-node traffic is excluded (via the node IP address set `$node_ips`).
- Link-local destinations are excluded.
- East-west traffic (pod-to-pod, pod-to-service) is excluded by higher-priority no-reroute policies at priority 102.

When source CIDRs are configured, one policy per CIDR per IP family is created.

#### Priority Hierarchy

The Egress Gateway integrates into the existing logical router policy priority scheme:

| Priority | Policy | Description |
|----------|--------|-------------|
| 102 | No-reroute (allow) | East-west traffic: pod-to-pod, pod-to-service, pod-to-node |
| 101 | EgressService reroute | Per-pod reroute for EgressService endpoints |
| 100 | EgressIP reroute | Per-pod reroute for pods matched by an EgressIP |
| 99 | EgressIP default reroute | Non-trafficSelector EgressIPs coexisting with trafficSelector EgressIPs |
| **98** | **Egress Gateway reroute** | **Catch-all: all remaining pod egress traffic** |

Because OVN evaluates policies from highest to lowest priority:
1. East-west traffic is allowed at priority 102 and never reaches lower policies.
2. Pods with an EgressIP are rerouted at priority 100 with their specific EgressIP SNAT.
3. All remaining pod traffic falls through to priority 98 and is rerouted to egress nodes.

#### Next Hop Selection

The next hops in the reroute policy depend on the zone topology of egress-assignable nodes:

- **Local-zone egress nodes**: The gateway router join port IP is used as the next hop (e.g., `100.64.0.3`).
- **Remote-zone egress nodes (Interconnect mode)**: The transit switch IP is used as the next hop, allowing traffic to reach egress nodes in remote zones.

All eligible egress node IPs are listed as next hops, enabling ECMP-based load distribution.

#### Dynamic Updates

The reroute policy is re-evaluated whenever egress-assignable nodes change:
- When a node gains the `k8s.ovn.org/egress-assignable` label, its gateway router IP is added to the next hops.
- When a node loses the label or is removed, its IP is dropped from the next hops.
- When no egress-assignable nodes remain, the policy is deleted entirely and traffic falls back to exiting via each pod's local node.

#### Traffic Flow

```none
                     +--------------------+
                     |                    |
                     |external destination|
                     |                    |
                     +---^----------------+
                         |
     4. packet exits     |
        with egress      |
        node's IP        |
        (masquerade)     |
                         |
                      +--+---+
                   +--+breth0+--+
                   |            |
                   +------------+
                   | ovn-worker |       2. priority 98 policy reroutes to
                   |  (egress)  |          ovn-worker's GW router IP
                   |            |       +------------------+
                   |  3. SNAT   |   +---+ovn cluster router|
                   |  (default  |   |   +----------^-------+
                   |masquerade) |   |              |
                   +------------+   |              |1. pod sends traffic to
                                    |              |   external destination
                             +------v+             |
                          +--+  GR   +--+          |   +----------------+
                          |  ovn-worker |          |   |  ovn-worker2   |
                          +-------------+          |   |                |
                                                   |   | +------------+ |
                                                   +---+-+    pod     | |
                                                       | | 10.244.1.3 | |
                                                       | +------------+ |
                                                       +----------------+
```

#### Differences Between Default and Interconnect Mode

In **default mode** (single zone), all egress-assignable nodes are local and the gateway router join port IP is used as next hops.

In **Interconnect mode** (multi-zone), egress-assignable nodes may be in remote zones. For remote-zone nodes, the transit switch IP is used as the next hop instead, allowing traffic to traverse zones to reach the egress node.

#### Differences Between LGW and SGW Modes

There are no differences between local gateway (LGW) and shared gateway (SGW) modes for this feature. The reroute policy operates at the logical router level, above the gateway mode distinction. Once traffic reaches the egress node's gateway router, the existing gateway mode logic handles SNAT and forwarding.

#### Feature Compatibility

##### EgressIP

The Egress Gateway is fully compatible with EgressIP. EgressIP reroute policies operate at priority 100, above the Egress Gateway's priority 98. When both features are enabled, pods matched by an EgressIP are rerouted and SNATed at priority 100, and the Egress Gateway catch-all at priority 98 only affects pods not matched by any EgressIP.

##### EgressService

EgressService reroute policies operate at priority 101. Pods that are endpoints of an EgressService are rerouted at that priority and are not affected by the Egress Gateway policy.

##### AdminPolicyBasedExternalRoute

AdminPolicyBasedExternalRoute (APB) reroute policies operate at priority 501, well above the Egress Gateway's priority 98. If an APB policy targets a namespace, its pods' egress traffic is rerouted to the configured external gateway at priority 501 and never reaches the Egress Gateway catch-all.

Importantly, APB also overrides EgressIP: because APB's priority 501 is higher than EgressIP's priority 100, a pod that has both an APB policy and an EgressIP will have its traffic rerouted by APB. The EgressIP SNAT will **not** be applied regardless of which node APB routes to:

- **APB routes to a different node than the EgressIP node**: The EgressIP SNAT NAT rule is installed on the EgressIP node's gateway router (`GR_<egressIP-node>`), so the destination node's gateway router has no such NAT rule. The traffic exits with default masquerade (the destination node's IP).
- **APB routes to the same node as the EgressIP node**: The EgressIP SNAT NAT rule still does not match. EgressIP NAT rules are created with a `logical_port` constraint set to the node's management port (`k8s-<node>`). Traffic rerouted by APB arrives at the gateway router via the join switch, not through the management port, so the `logical_port` condition is not satisfied and the EgressIP SNAT is not applied. The traffic exits with default masquerade.

### Testing Details

#### Unit Testing

Unit tests for the `EgressGatewayController` are located in `go-controller/pkg/ovn/egressip_test.go` under the `"Egress Gateway - ensureReroutePolicy"` context. These tests validate:
- Policy creation at the correct priority when egress-assignable nodes exist.
- Policy creation with configured source CIDRs instead of cluster subnets.
- Correct match expressions including node IP exclusion and link-local exclusion.
- ECMP next hop list construction.
- Policy removal when no egress-assignable nodes exist.
- Correct external IDs for policy identification.

#### E2E Testing

E2E tests are located in the `test/e2e/` directory and exercise the feature in a kind cluster. They validate:
- Pod egress traffic is routed through egress-assignable nodes.
- Traffic source IP is the egress node's IP (masquerade verification).
- Coexistence with EgressIP: pods matched by an EgressIP are not affected by the Egress Gateway.
- Dynamic behavior when nodes gain or lose the `k8s.ovn.org/egress-assignable` label.
- Fallback to local node egress when no egress-assignable nodes exist.

The kind.sh helper script supports enabling the feature for E2E testing:
```shell
./kind.sh --egress-gateway-enable
```

### Documentation Details

Feature documentation is available at `docs/features/cluster-egress-controls/egress-gateway.md` covering:
- Prerequisites and configuration.
- Traffic flow scenarios with diagrams.
- Implementation details including OVN logical router policies.
- Interaction with EgressIP and EgressService features.

## Risks, Known Limitations and Mitigations

1. **Single point of failure**: If all egress-assignable nodes go down, the reroute policy is removed and traffic falls back to local node egress. Administrators should ensure sufficient egress node redundancy.

2. **ECMP load distribution is not session-aware**: OVN ECMP distributes traffic across egress nodes per-flow, but there is no guarantee of session affinity. External systems that rely on consistent source IPs per session should use EgressIP instead.

3. **No per-pod granularity**: The Egress Gateway steers all pod traffic (or all traffic from configured source CIDRs). For per-pod control, EgressIP should be used in conjunction with this feature.

4. **Source CIDRs are static**: The `--egress-gateway-source-cidrs` flag is set at startup and requires a restart to change. Dynamic source CIDR configuration is not supported.

## OVN-Kubernetes Version Skew

This feature is planned for introduction in the current development cycle.

## Backwards Compatibility

- The feature is disabled by default (`--enable-egress-gateway=false`), so existing deployments are unaffected.
- No existing APIs or CRDs are modified.
- When enabled, the feature adds a new logical router policy at priority 98. This priority is below all existing reroute policies (EgressIP at 100, EgressService at 101), so existing behavior for those features is preserved.
- The `k8s.ovn.org/egress-assignable` node label is shared with the EgressIP feature. Nodes labeled for EgressIP can also serve as egress gateway nodes when the feature is enabled.

## Alternatives

1. **AdminPolicyBasedExternalRoute (APB)**: Administrators could create APB policies selecting all namespaces with static hops pointing to cluster node IPs. However, APB operates at priority 501 which overrides EgressIP and EgressService, so it cannot serve as a low-priority catch-all. Additionally, APB requires manual management of static hop IPs when nodes are added or removed, whereas the Egress Gateway dynamically tracks nodes via the `k8s.ovn.org/egress-assignable` label.

2. **EgressIP for all pods**: Administrators could create EgressIP objects to cover all pods in the cluster. However, this requires per-namespace configuration, is operationally complex at scale, and is not designed as a catch-all mechanism.

3. **External network-level routing**: Traffic steering could be done outside of OVN using external routers or network policies. This requires infrastructure-level changes and does not integrate with the Kubernetes control plane.

4. **iptables-based SNAT on designated nodes**: An operator could configure iptables rules to SNAT traffic on specific nodes. This bypasses OVN's logical routing and does not integrate with the existing OVN policy hierarchy.

## References

- Feature documentation: `docs/features/cluster-egress-controls/egress-gateway.md`
- EgressIP feature: `docs/features/cluster-egress-controls/egress-ip.md`
