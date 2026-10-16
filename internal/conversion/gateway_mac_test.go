// SPDX-License-Identifier:Apache-2.0

package conversion

import (
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openperouter/openperouter/api/v1alpha1"
)

func TestFilterValidL2VNIsGatewayMAC(t *testing.T) {
	tests := []struct {
		name       string
		mac        string
		noGateway  bool
		wantErrStr string
	}{
		{name: "unset", mac: ""},
		{name: "unset without gatewayIPs", mac: "", noGateway: true},
		{name: "IANA anycast-gw MAC", mac: "00:00:5e:00:01:01"},
		{name: "upper case", mac: "00:00:5E:00:01:0A"},
		{name: "locally administered", mac: "02:00:00:00:00:01"},
		{name: "multicast", mac: "01:00:5e:00:00:01", wantErrStr: "must be unicast"},
		{name: "multicast, other octet", mac: "33:33:00:00:00:01", wantErrStr: "must be unicast"},
		{name: "broadcast", mac: "ff:ff:ff:ff:ff:ff", wantErrStr: "must not be the broadcast address"},
		{name: "all zeros", mac: "00:00:00:00:00:00", wantErrStr: "must not be all zeros"},
		{name: "hyphen form", mac: "00-00-5e-00-01-01", wantErrStr: "48-bit MAC in colon form"},
		{name: "dotted form", mac: "0000.5e00.0101", wantErrStr: "48-bit MAC in colon form"},
		{name: "EUI-64", mac: "00:00:5e:00:01:01:00:00", wantErrStr: "48-bit MAC in colon form"},
		{name: "too short", mac: "00:00:5e:00:01", wantErrStr: "48-bit MAC in colon form"},
		{name: "not hex", mac: "00:00:5e:00:01:0g", wantErrStr: "48-bit MAC in colon form"},
		{name: "without gatewayIPs", mac: "00:00:5e:00:01:01", noGateway: true,
			wantErrStr: "gatewayMAC requires gatewayIPs"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l2 := gatewayMACL2VNI("net-1", 30201, "192.168.1.1/24", tc.mac)
			if tc.noGateway {
				l2.Spec.GatewayIPs = nil
				l2.Spec.RoutingDomain = nil
			}

			valid, err := FilterValidL2VNIs([]v1alpha1.L2VNI{l2})

			if tc.wantErrStr == "" {
				if err != nil || len(valid) != 1 {
					t.Fatalf("got %d valid and error %v, want the L2VNI accepted", len(valid), err)
				}
				return
			}
			if len(valid) != 0 {
				t.Errorf("the L2VNI with gatewayMAC %q was kept", tc.mac)
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErrStr) {
				t.Errorf("got error %v, want one containing %q", err, tc.wantErrStr)
			}
		})
	}
}

// A bad gatewayMAC leaves out that L2VNI alone, the way a bad route target does.
func TestFilterValidL2VNIsBadGatewayMACLeavesOutOnlyThatL2VNI(t *testing.T) {
	l2vnis := []v1alpha1.L2VNI{
		gatewayMACL2VNI("net-1", 30201, "192.168.1.1/24", "00:00:5e:00:01:01"),
		gatewayMACL2VNI("net-bad", 30202, "192.168.2.1/24", "01:00:5e:00:01:01"),
		gatewayMACL2VNI("net-3", 30203, "192.168.3.1/24", ""),
	}

	valid, err := FilterValidL2VNIs(l2vnis)

	names := make([]string, 0, len(valid))
	for _, l2 := range valid {
		names = append(names, l2.Name)
	}
	if fmt.Sprint(names) != "[net-1 net-3]" {
		t.Errorf("valid L2VNIs = %v, want [net-1 net-3]", names)
	}
	if err == nil || !strings.Contains(err.Error(), "net-bad") {
		t.Errorf("got error %v, want one naming net-bad", err)
	}
}

// Every network of a fabric has the same anycast gateway MAC, and each L2VNI
// has its own bridge: neither the same MAC on many L2VNIs of one node nor
// different MACs on one node is a conflict.
func TestValidateOverlayResourcesForNodesGatewayMACs(t *testing.T) {
	node := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1"}}
	l3vnis := []v1alpha1.L3VNI{{
		ObjectMeta: metav1.ObjectMeta{Name: "red"},
		Spec:       v1alpha1.L3VNISpec{VNI: 10001, VRF: "red"},
	}}
	tests := []struct {
		name   string
		l2vnis []v1alpha1.L2VNI
	}{
		{
			name: "two L2VNIs with different MACs",
			l2vnis: []v1alpha1.L2VNI{
				gatewayMACL2VNI("net-1", 30201, "192.168.1.1/24", "00:00:5e:00:01:01"),
				gatewayMACL2VNI("net-2", 30202, "192.168.2.1/24", "00:00:5e:00:01:02"),
			},
		},
		{
			name: "many L2VNIs with the same MAC",
			l2vnis: func() []v1alpha1.L2VNI {
				l2s := make([]v1alpha1.L2VNI, 0, 5)
				for i := range int32(5) {
					l2s = append(l2s, gatewayMACL2VNI(fmt.Sprintf("net-%d", i), 30201+i,
						fmt.Sprintf("192.168.%d.1/24", i+1), "00:00:5e:00:01:01"))
				}
				return l2s
			}(),
		},
		{
			name: "one with a MAC beside one without",
			l2vnis: []v1alpha1.L2VNI{
				gatewayMACL2VNI("net-1", 30201, "192.168.1.1/24", "00:00:5e:00:01:01"),
				gatewayMACL2VNI("net-2", 30202, "192.168.2.1/24", ""),
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateOverlayResourcesForNodes([]corev1.Node{node}, tc.l2vnis, l3vnis, nil); err != nil {
				t.Errorf("got %v, want no error", err)
			}
		})
	}
}

func gatewayMACL2VNI(name string, vni int32, gateway, mac string) v1alpha1.L2VNI {
	return v1alpha1.L2VNI{
		ObjectMeta: metav1.ObjectMeta{Namespace: "openperouter-system", Name: name},
		Spec: v1alpha1.L2VNISpec{
			VNI:        vni,
			GatewayIPs: []string{gateway},
			GatewayMAC: optionalMAC(mac),
			RoutingDomain: &v1alpha1.RoutingDomain{
				Type:  v1alpha1.RoutingDomainTypeL3VNI,
				L3VNI: &v1alpha1.L3VNIReference{Name: "red"},
			},
		},
	}
}

// optionalMAC returns nil for "", as for an unset field.
func optionalMAC(mac string) *string {
	if mac == "" {
		return nil
	}
	return &mac
}
