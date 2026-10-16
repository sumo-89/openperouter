// SPDX-License-Identifier:Apache-2.0

package conversion

import (
	"net"
	"testing"

	"github.com/openperouter/openperouter/api/v1alpha1"
	"github.com/openperouter/openperouter/internal/hostnetwork"
)

func TestL2VNIToHostCarriesTheGatewayMAC(t *testing.T) {
	vtepIPs := tunnelVtepIPs{forL2VNIs: map[string]string{"net-1": "192.0.2.10/32"}}
	vrfs := map[string]string{v1alpha1.RoutingDomainTypeL3VNI + "/red": "red"}
	overheads := map[string]int{"net-1": hostnetwork.VXLanOverhead}

	tests := []struct {
		name string
		mac  string
		want net.HardwareAddr
	}{
		{name: "explicit", mac: "00:00:5e:00:01:01", want: net.HardwareAddr{0x00, 0x00, 0x5e, 0x00, 0x01, 0x01}},
		{name: "upper case", mac: "00:00:5E:00:01:0A", want: net.HardwareAddr{0x00, 0x00, 0x5e, 0x00, 0x01, 0x0a}},
		// Unset carries nothing, and SetupL2VNI derives the MAC from the VNI.
		{name: "unset", mac: "", want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l2 := gatewayMACL2VNI("net-1", 30201, "192.168.1.1/24", tc.mac)

			got, err := l2vniToHost(l2, vtepIPs, "/var/run/netns/perouter", vrfs, overheads)
			if err != nil {
				t.Fatalf("l2vniToHost: %v", err)
			}
			if got.GatewayMAC.String() != tc.want.String() {
				t.Errorf("GatewayMAC = %q, want %q", got.GatewayMAC, tc.want)
			}
		})
	}
}
