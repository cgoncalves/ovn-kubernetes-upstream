package egressip

import (
	"net"
	"syscall"
	"testing"

	egressiptrafficv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressiptraffic/v1"
)

func stringPtr(s string) *string { return &s }
func int32Ptr(i int32) *int32    { return &i }

func TestParseTrafficMatchers(t *testing.T) {
	tests := []struct {
		name       string
		matchers   []egressiptrafficv1.TrafficMatcher
		wantLen    int
		wantErr    bool
		checkFirst func(t *testing.T, m *ParsedTrafficMatcher)
	}{
		{
			name: "CIDR only",
			matchers: []egressiptrafficv1.TrafficMatcher{
				{CIDR: "192.168.250.0/24"},
			},
			wantLen: 1,
			checkFirst: func(t *testing.T, m *ParsedTrafficMatcher) {
				if m.CIDR.String() != "192.168.250.0/24" {
					t.Errorf("got CIDR %s, want 192.168.250.0/24", m.CIDR.String())
				}
				if m.Protocol != 0 {
					t.Errorf("got Protocol %d, want 0", m.Protocol)
				}
				if m.HasL4Filter() {
					t.Error("CIDR-only matcher should not have L4 filter")
				}
			},
		},
		{
			name: "TCP with single destination port",
			matchers: []egressiptrafficv1.TrafficMatcher{
				{CIDR: "10.0.0.0/8", Protocol: stringPtr("TCP"), DestinationPort: int32Ptr(5060)},
			},
			wantLen: 1,
			checkFirst: func(t *testing.T, m *ParsedTrafficMatcher) {
				if m.Protocol != syscall.IPPROTO_TCP {
					t.Errorf("got Protocol %d, want %d", m.Protocol, syscall.IPPROTO_TCP)
				}
				if m.DstPortStart != 5060 || m.DstPortEnd != 5060 {
					t.Errorf("got DstPort %d-%d, want 5060-5060", m.DstPortStart, m.DstPortEnd)
				}
				if !m.HasL4Filter() {
					t.Error("TCP+port matcher should have L4 filter")
				}
				if m.ProtocolName() != "tcp" {
					t.Errorf("got ProtocolName %q, want tcp", m.ProtocolName())
				}
			},
		},
		{
			name: "UDP with destination port range",
			matchers: []egressiptrafficv1.TrafficMatcher{
				{
					CIDR:     "10.0.0.0/8",
					Protocol: stringPtr("UDP"),
					DestinationPortRange: &egressiptrafficv1.PortRange{
						Start: 16384,
						End:   16399,
					},
				},
			},
			wantLen: 1,
			checkFirst: func(t *testing.T, m *ParsedTrafficMatcher) {
				if m.Protocol != syscall.IPPROTO_UDP {
					t.Errorf("got Protocol %d, want %d", m.Protocol, syscall.IPPROTO_UDP)
				}
				if m.DstPortStart != 16384 || m.DstPortEnd != 16399 {
					t.Errorf("got DstPort %d-%d, want 16384-16399", m.DstPortStart, m.DstPortEnd)
				}
			},
		},
		{
			name: "SCTP with source port",
			matchers: []egressiptrafficv1.TrafficMatcher{
				{CIDR: "10.0.0.0/8", Protocol: stringPtr("SCTP"), SourcePort: int32Ptr(3868)},
			},
			wantLen: 1,
			checkFirst: func(t *testing.T, m *ParsedTrafficMatcher) {
				if m.Protocol != syscall.IPPROTO_SCTP {
					t.Errorf("got Protocol %d, want %d", m.Protocol, syscall.IPPROTO_SCTP)
				}
				if m.SrcPortStart != 3868 || m.SrcPortEnd != 3868 {
					t.Errorf("got SrcPort %d-%d, want 3868-3868", m.SrcPortStart, m.SrcPortEnd)
				}
				if m.DstPortStart != 0 {
					t.Error("DstPortStart should be 0 when not set")
				}
			},
		},
		{
			name: "TCP with both dst and src port ranges",
			matchers: []egressiptrafficv1.TrafficMatcher{
				{
					CIDR:                 "10.0.0.0/8",
					Protocol:             stringPtr("TCP"),
					DestinationPort:      int32Ptr(22),
					SourcePortRange:      &egressiptrafficv1.PortRange{Start: 1024, End: 1026},
				},
			},
			wantLen: 1,
			checkFirst: func(t *testing.T, m *ParsedTrafficMatcher) {
				if m.DstPortStart != 22 || m.DstPortEnd != 22 {
					t.Errorf("got DstPort %d-%d, want 22-22", m.DstPortStart, m.DstPortEnd)
				}
				if m.SrcPortStart != 1024 || m.SrcPortEnd != 1026 {
					t.Errorf("got SrcPort %d-%d, want 1024-1026", m.SrcPortStart, m.SrcPortEnd)
				}
			},
		},
		{
			name: "invalid CIDR",
			matchers: []egressiptrafficv1.TrafficMatcher{
				{CIDR: "not-a-cidr"},
			},
			wantErr: true,
		},
		{
			name: "unsupported protocol",
			matchers: []egressiptrafficv1.TrafficMatcher{
				{CIDR: "10.0.0.0/8", Protocol: stringPtr("ICMP")},
			},
			wantErr: true,
		},
		{
			name: "multiple matchers",
			matchers: []egressiptrafficv1.TrafficMatcher{
				{CIDR: "192.168.250.0/24"},
				{CIDR: "10.0.0.0/8", Protocol: stringPtr("TCP"), DestinationPort: int32Ptr(80)},
			},
			wantLen: 2,
		},
		{
			name:     "empty matchers",
			matchers: nil,
			wantLen:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ParseTrafficMatchers(tt.matchers)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseTrafficMatchers() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}
			if len(result) != tt.wantLen {
				t.Errorf("got %d matchers, want %d", len(result), tt.wantLen)
				return
			}
			if tt.checkFirst != nil && len(result) > 0 {
				tt.checkFirst(t, result[0])
			}
		})
	}
}

func TestUniqueCIDRs(t *testing.T) {
	cidr1 := mustParseCIDR("192.168.250.0/24")
	cidr2 := mustParseCIDR("10.0.0.0/8")

	matchers := []*ParsedTrafficMatcher{
		{CIDR: cidr1, Protocol: syscall.IPPROTO_TCP, DstPortStart: 80, DstPortEnd: 80},
		{CIDR: cidr1, Protocol: syscall.IPPROTO_UDP, DstPortStart: 53, DstPortEnd: 53},
		{CIDR: cidr2},
		{CIDR: cidr1}, // duplicate
	}

	result := UniqueCIDRs(matchers)
	if len(result) != 2 {
		t.Errorf("got %d unique CIDRs, want 2", len(result))
	}
}

func TestSplitByL4(t *testing.T) {
	cidr1 := mustParseCIDR("192.168.250.0/24")
	cidr2 := mustParseCIDR("10.0.0.0/8")

	matchers := []*ParsedTrafficMatcher{
		{CIDR: cidr1},                                                                  // CIDR-only
		{CIDR: cidr2, Protocol: syscall.IPPROTO_TCP, DstPortStart: 80, DstPortEnd: 80}, // L4
		{CIDR: cidr1, Protocol: syscall.IPPROTO_UDP},                                   // L4 (protocol only, no port)
	}

	cidrOnly, withL4 := SplitByL4(matchers)
	if len(cidrOnly) != 1 {
		t.Errorf("got %d CIDR-only, want 1", len(cidrOnly))
	}
	if len(withL4) != 2 {
		t.Errorf("got %d with-L4, want 2", len(withL4))
	}
}

func TestHasL4Filter(t *testing.T) {
	tests := []struct {
		name   string
		m      ParsedTrafficMatcher
		wantL4 bool
	}{
		{"no protocol", ParsedTrafficMatcher{CIDR: mustParseCIDR("10.0.0.0/8")}, false},
		{"protocol only", ParsedTrafficMatcher{CIDR: mustParseCIDR("10.0.0.0/8"), Protocol: syscall.IPPROTO_TCP}, true},
		{"protocol + port", ParsedTrafficMatcher{CIDR: mustParseCIDR("10.0.0.0/8"), Protocol: syscall.IPPROTO_TCP, DstPortStart: 80, DstPortEnd: 80}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.m.HasL4Filter(); got != tt.wantL4 {
				t.Errorf("HasL4Filter() = %v, want %v", got, tt.wantL4)
			}
		})
	}
}

func TestProtocolName(t *testing.T) {
	tests := []struct {
		proto int
		want  string
	}{
		{syscall.IPPROTO_TCP, "tcp"},
		{syscall.IPPROTO_UDP, "udp"},
		{syscall.IPPROTO_SCTP, "sctp"},
		{0, ""},
	}
	for _, tt := range tests {
		m := ParsedTrafficMatcher{Protocol: tt.proto}
		if got := m.ProtocolName(); got != tt.want {
			t.Errorf("ProtocolName(%d) = %q, want %q", tt.proto, got, tt.want)
		}
	}
}

func mustParseCIDR(s string) *net.IPNet {
	_, ipNet, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return ipNet
}
