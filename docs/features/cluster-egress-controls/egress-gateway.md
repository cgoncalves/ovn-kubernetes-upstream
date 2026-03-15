# Egress Gateway

## Introduction

The Egress Gateway feature ensures that **all** pod egress traffic in a cluster is routed through designated egress nodes — nodes labeled with `k8s.ovn.org/egress-assignable`. Traffic is rerouted to an egress node, where it is SNATed with the egress node's own IP address (default masquerade). This provides a predictable and controllable egress point for every pod in the cluster.

The Egress Gateway is an independent feature that creates a lower-priority catch-all reroute policy to steer all pod egress traffic to egress-assignable nodes. It is compatible with EgressIP: when both are enabled, EgressIP reroutes traffic for *specific* pods at a higher priority, while the Egress Gateway handles *all remaining* pods. East-west traffic (pod-to-pod, pod-to-service, pod-to-node) is not affected.

## Prerequisites

- At least one node must be labeled as egress-assignable:
```shell
kubectl label nodes <node_name> k8s.ovn.org/egress-assignable=""
```

## Configuration

The feature is disabled by default and must be explicitly enabled via the `--enable-egress-gateway` flag.

This can be set in the following ways:
- ovnkube binary flag: `--enable-egress-gateway`
- inside config specified by `--config-file` flag:
```
[ovnkubernetesfeature]
enable-egress-gateway=true
```

### Source CIDRs

By default, the Egress Gateway steers egress traffic from **all** cluster pod subnets (catch-all). To steer only specific source CIDRs, use the `--egress-gateway-source-cidrs` flag:

```
--egress-gateway-source-cidrs=10.132.0.0/14,10.136.0.0/14
```

Or in the config file:
```
[ovnkubernetesfeature]
enable-egress-gateway=true
egress-gateway-source-cidrs=10.132.0.0/14,10.136.0.0/14
```

When set, only pods with IPs matching the specified CIDRs have their egress traffic steered through egress nodes. All other pods' egress traffic exits via their local node as usual. Both IPv4 and IPv6 CIDRs are supported.

### Enabling in a kind cluster

Using the `kind.sh` helper script:
```shell
./kind.sh --egress-gateway-enable
```
or equivalently:
```shell
ENABLE_EGRESS_GATEWAY=true ./kind.sh
```

To also set source CIDRs:
```shell
./kind.sh --egress-gateway-enable --egress-gateway-source-cidrs 10.132.0.0/14
```

## How it works

When enabled, the Egress Gateway creates a logical router policy on `ovn_cluster_router` at **priority 99** (`EgressGatewayReroutePriority`) that matches pod subnet traffic and reroutes it to egress-assignable nodes. The match expression includes exclusions for pod-to-node traffic (via the node IP address set) and link-local destinations, ensuring only external-bound traffic is steered. Pod-to-pod and pod-to-service traffic is excluded by higher-priority no-reroute policies at priority 102.

This establishes a clear priority hierarchy:

| Priority | Policy | Description |
|----------|--------|-------------|
| 102 | No-reroute (allow) | East-west traffic: pod-to-pod, pod-to-service, pod-to-node |
| 101 | EgressService reroute | Per-pod reroute for EgressService endpoints |
| 100 | EgressIP reroute | Per-pod reroute for pods matched by an EgressIP |
| **99** | **Egress Gateway reroute** | **Catch-all: all remaining pod egress traffic** |

Because OVN evaluates policies from highest to lowest priority, the behavior is:

1. East-west traffic is allowed at priority 102 and never reaches lower policies.
2. Pods with an EgressIP are rerouted at priority 100 with their specific EgressIP SNAT.
3. All remaining pod traffic falls through to priority 99 and is rerouted to egress nodes.

When no egress-assignable nodes exist, the priority 99 policy is removed and traffic exits via the local node as usual.

## Traffic scenarios

### Scenario 1: Pod egress traffic steered through egress node

A pod's external traffic matches the catch-all priority 99 policy and is rerouted to one of the egress-assignable nodes via ECMP. On the egress node, default masquerade applies and the traffic exits with the egress node's IP as the source.

```none
                     ┌────────────────────┐
                     │                    │
                     │external destination│
                     │                    │
                     └───▲────────────────┘
                         │
     4. packet exits     │
        with egress      │
        node's IP        │
        (masquerade)     │
                         │
                      ┌──┴───┐
                   ┌──┘breth0└──┐
                   │            │
                   ├────────────┤
                   │ ovn-worker │       2. priority 99 policy reroutes to
                   │  (egress)  │          ovn-worker's GW router IP
                   │            │       ┌──────────────────┐
                   │  3. SNAT   │   ┌───┤ovn cluster router│
                   │  (default  │   │   └───────────▲──────┘
                   │masquerade) │   │               │
                   └────────────┘   │               │1. pod sends traffic to
                                    │               │   external destination
                             ┌──────▼┐              │
                          ┌──┘  GR   └──┐           │   ┌────────────────┐
                          │  ovn-worker │           │   │  ovn-worker2   │
                          └─────────────┘           │   │                │
                                                    │   │ ┌────────────┐ │
                                                    └───┼─┤    pod     │ │
                                                        │ │ 10.244.1.3 │ │
                                                        │ └────────────┘ │
                                                        └────────────────┘
```

### Scenario 2: Pod with EgressIP (when both features are enabled)

A pod matched by an EgressIP. This pod's traffic is rerouted at **priority 100** (the standard EgressIP per-pod policy), which takes precedence over the priority 99 Egress Gateway policy. The traffic is SNATed with the configured EgressIP address and never reaches the Egress Gateway catch-all.

## Implementation details

### Logical router policies

With the Egress Gateway enabled on a cluster with pod subnet `10.244.0.0/16` and two egress-assignable nodes (`ovn-worker` and `ovn-worker2`), the `ovn_cluster_router` policies look like:

```shell
$ ovn-nbctl lr-policy-list ovn_cluster_router
Routing Policies
  1004 inport == "rtos-ovn-control-plane" && ip4.dst == 172.18.0.4 /* ovn-control-plane */    reroute  10.244.0.2
  1004 inport == "rtos-ovn-worker" && ip4.dst == 172.18.0.2 /* ovn-worker */                  reroute  10.244.1.2
  1004 inport == "rtos-ovn-worker2" && ip4.dst == 172.18.0.3 /* ovn-worker2 */                reroute  10.244.2.2

   102 ip4.src == 10.244.0.0/16 && ip4.dst == 10.244.0.0/16                                   allow
   102 ip4.src == 10.244.0.0/16 && ip4.dst == 100.64.0.0/16                                   allow

    99 ip4.src == 10.244.0.0/16 && ip4.dst != $node_ips && ip4.dst != 169.254.0.0/16          reroute  100.64.0.3, 100.64.0.4
```

- Rules at `1004` handle pod-to-local-host-IP traffic.
- Rules at `102` ensure east-west traffic (pod-to-pod, pod-to-join-subnet) is not rerouted.
- The rule at `99` catches all remaining pod egress traffic and reroutes it to the gateway router IPs of the egress-assignable nodes using ECMP for load balancing. The match excludes pod-to-node traffic (`ip4.dst != $node_ips`) and link-local destinations (`ip4.dst != 169.254.0.0/16`).

If a pod also has an EgressIP configured, the per-pod policy at priority 100 appears:
```shell
   100 ip4.src == 10.244.1.3                                                                  reroute  100.64.0.3, 100.64.0.4
    99 ip4.src == 10.244.0.0/16 && ip4.dst != $node_ips && ip4.dst != 169.254.0.0/16          reroute  100.64.0.3, 100.64.0.4
```
The pod's traffic matches the priority 100 rule first and never reaches priority 99.

### Next hop selection

The next hops in the priority 99 reroute policy are determined based on the zone topology of egress-assignable nodes:

- **Local-zone egress nodes**: the gateway router join port IP is used as the next hop (e.g., `100.64.0.3`).
- **Remote-zone egress nodes (Interconnect mode)**: the transit switch IP is used as the next hop, allowing traffic to reach egress nodes in remote zones.

All eligible egress node IPs are listed as next hops, enabling ECMP-based load distribution across egress nodes.

### Dynamic updates

The priority 99 policy is re-evaluated whenever egress-assignable nodes change:
- When a new node is labeled `k8s.ovn.org/egress-assignable`, its gateway router IP is added to the next hops.
- When a node loses the label or is removed, its IP is dropped from the next hops.
- When no egress-assignable nodes remain, the policy is deleted entirely and traffic falls back to exiting via each pod's local node.

## Interaction with other features

### EgressIP

The Egress Gateway is independent from the EgressIP feature — neither requires the other. When both are enabled, they are fully compatible: pods matched by an EgressIP are handled by the per-pod reroute at priority 100, while the priority 99 gateway policy only affects pods that are **not** matched by any EgressIP. EgressIP provides fine-grained per-pod SNAT control, while the Egress Gateway provides a cluster-wide catch-all.

### EgressService

EgressService reroutes operate at priority 101. Pods that are endpoints of an EgressService are rerouted at that priority and are not affected by the priority 99 gateway policy.


