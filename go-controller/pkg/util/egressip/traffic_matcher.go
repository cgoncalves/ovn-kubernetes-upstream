package egressip

import (
	"fmt"
	"net"
	"strings"
	"syscall"

	egressiptrafficv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressiptraffic/v1"
)

// ParsedTrafficMatcher represents a parsed traffic matcher from EgressIPTraffic
// with validated CIDR and optional L4 protocol/port fields.
type ParsedTrafficMatcher struct {
	CIDR         *net.IPNet
	Protocol     int    // syscall.IPPROTO_TCP etc, 0 = not set
	DstPortStart uint16 // 0 = not set
	DstPortEnd   uint16
	SrcPortStart uint16 // 0 = not set
	SrcPortEnd   uint16
}

// HasL4Filter returns true if this matcher has any L4 protocol/port filtering.
func (m *ParsedTrafficMatcher) HasL4Filter() bool {
	return m.Protocol != 0
}

// ProtocolName returns the lowercase protocol name for OVN match expressions.
func (m *ParsedTrafficMatcher) ProtocolName() string {
	switch m.Protocol {
	case syscall.IPPROTO_TCP:
		return "tcp"
	case syscall.IPPROTO_UDP:
		return "udp"
	case syscall.IPPROTO_SCTP:
		return "sctp"
	default:
		return ""
	}
}

// ParseTrafficMatchers converts CRD TrafficMatcher entries to internal representation,
// validating CIDRs and converting protocol strings to integer constants.
func ParseTrafficMatchers(matchers []egressiptrafficv1.TrafficMatcher) ([]*ParsedTrafficMatcher, error) {
	var result []*ParsedTrafficMatcher
	for _, m := range matchers {
		_, ipNet, err := net.ParseCIDR(m.CIDR)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q: %v", m.CIDR, err)
		}
		parsed := &ParsedTrafficMatcher{CIDR: ipNet}

		if m.Protocol != nil {
			switch strings.ToUpper(*m.Protocol) {
			case "TCP":
				parsed.Protocol = syscall.IPPROTO_TCP
			case "UDP":
				parsed.Protocol = syscall.IPPROTO_UDP
			case "SCTP":
				parsed.Protocol = syscall.IPPROTO_SCTP
			default:
				return nil, fmt.Errorf("unsupported protocol %q", *m.Protocol)
			}
		}

		if m.DestinationPort != nil {
			parsed.DstPortStart = uint16(*m.DestinationPort)
			parsed.DstPortEnd = parsed.DstPortStart
		} else if m.DestinationPortRange != nil {
			parsed.DstPortStart = uint16(m.DestinationPortRange.Start)
			parsed.DstPortEnd = uint16(m.DestinationPortRange.End)
		}

		if m.SourcePort != nil {
			parsed.SrcPortStart = uint16(*m.SourcePort)
			parsed.SrcPortEnd = parsed.SrcPortStart
		} else if m.SourcePortRange != nil {
			parsed.SrcPortStart = uint16(m.SourcePortRange.Start)
			parsed.SrcPortEnd = uint16(m.SourcePortRange.End)
		}

		result = append(result, parsed)
	}
	return result, nil
}

// UniqueCIDRs returns deduplicated CIDRs from parsed matchers, suitable for route creation.
func UniqueCIDRs(matchers []*ParsedTrafficMatcher) []*net.IPNet {
	seen := make(map[string]struct{})
	var result []*net.IPNet
	for _, m := range matchers {
		key := m.CIDR.String()
		if _, ok := seen[key]; !ok {
			seen[key] = struct{}{}
			result = append(result, m.CIDR)
		}
	}
	return result
}

// SplitByL4 separates matchers into CIDR-only and CIDR+L4 groups.
func SplitByL4(matchers []*ParsedTrafficMatcher) (cidrOnly []*ParsedTrafficMatcher, withL4 []*ParsedTrafficMatcher) {
	for _, m := range matchers {
		if m.HasL4Filter() {
			withL4 = append(withL4, m)
		} else {
			cidrOnly = append(cidrOnly, m)
		}
	}
	return cidrOnly, withL4
}
