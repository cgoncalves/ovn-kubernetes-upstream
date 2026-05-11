package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +genclient:nonNamespaced
// +kubebuilder:object:root=true
// +kubebuilder:subresource:nostatus
// +kubebuilder:resource:shortName=eipt,scope=Cluster
// +kubebuilder:printcolumn:name="TrafficMatchers",type=string,JSONPath=".spec.trafficMatchers"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// EgressIPTraffic defines traffic matching rules for destination-based egress IP routing.
// Traffic matchers specify destination CIDRs with optional L4 protocol/port filtering.
// When selected by an EgressIP's TrafficSelector, only traffic matching these rules
// is routed through the EgressIP.
type EgressIPTraffic struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`

	Spec EgressIPTrafficSpec `json:"spec"`
}

// EgressIPTrafficSpec defines the desired state description of EgressIPTraffic.
type EgressIPTrafficSpec struct {
	// TrafficMatchers defines destination networks with optional L4 protocol/port filtering.
	// Each matcher specifies a destination CIDR and optionally restricts matching to a
	// specific IP protocol and destination/source port or port range.
	// Matchers without protocol/port fields match all traffic to the specified CIDR.
	// +optional
	// +kubebuilder:validation:MaxItems=25
	TrafficMatchers []TrafficMatcher `json:"trafficMatchers,omitempty"`
}

// TrafficMatcher defines a destination CIDR with optional L4 protocol and port filtering.
// +kubebuilder:validation:XValidation:rule="!has(self.destinationPort) || !has(self.destinationPortRange)",message="destinationPort and destinationPortRange are mutually exclusive"
// +kubebuilder:validation:XValidation:rule="!has(self.sourcePort) || !has(self.sourcePortRange)",message="sourcePort and sourcePortRange are mutually exclusive"
// +kubebuilder:validation:XValidation:rule="(!has(self.destinationPort) && !has(self.destinationPortRange) && !has(self.sourcePort) && !has(self.sourcePortRange)) || has(self.protocol)",message="protocol is required when any port field is set"
type TrafficMatcher struct {
	// CIDR is the destination network in IPv4 or IPv6 CIDR notation.
	// +kubebuilder:validation:MaxLength=43
	// +kubebuilder:validation:XValidation:rule="isCIDR(self)",message="must be valid IPv4 or IPv6 CIDR"
	CIDR string `json:"cidr"`

	// Protocol is the IP protocol to match (TCP, UDP, SCTP).
	// Required when any port field is set.
	// +optional
	// +kubebuilder:validation:Enum=TCP;UDP;SCTP
	Protocol *string `json:"protocol,omitempty"`

	// DestinationPort is a single destination port to match.
	// Mutually exclusive with DestinationPortRange.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	DestinationPort *int32 `json:"destinationPort,omitempty"`

	// DestinationPortRange specifies an inclusive range of destination ports.
	// Mutually exclusive with DestinationPort.
	// +optional
	DestinationPortRange *PortRange `json:"destinationPortRange,omitempty"`

	// SourcePort is a single source port to match.
	// Mutually exclusive with SourcePortRange.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	SourcePort *int32 `json:"sourcePort,omitempty"`

	// SourcePortRange specifies an inclusive range of source ports.
	// Mutually exclusive with SourcePort.
	// +optional
	SourcePortRange *PortRange `json:"sourcePortRange,omitempty"`
}

// PortRange defines an inclusive port range.
// +kubebuilder:validation:XValidation:rule="self.end >= self.start",message="end must be greater than or equal to start"
type PortRange struct {
	// Start is the first port in the range (inclusive).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Start int32 `json:"start"`

	// End is the last port in the range (inclusive).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	End int32 `json:"end"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// EgressIPTrafficList contains a list of EgressIPTraffic
type EgressIPTrafficList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []EgressIPTraffic `json:"items"`
}
