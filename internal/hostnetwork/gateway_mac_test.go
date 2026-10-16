// SPDX-License-Identifier:Apache-2.0

package hostnetwork

import (
	"net"
	"testing"
)

func TestGatewayMAC(t *testing.T) {
	explicit := net.HardwareAddr{0x00, 0x00, 0x5e, 0x00, 0x01, 0x01}
	tests := []struct {
		name   string
		params L2VNIParams
		want   string
	}{
		{
			name:   "default is derived from the VNI",
			params: L2VNIParams{VNIParams: VNIParams{VNI: 100}},
			want:   "00:f3:00:00:00:65",
		},
		{
			name:   "configured",
			params: L2VNIParams{VNIParams: VNIParams{VNI: 100}, GatewayMAC: explicit},
			want:   "00:00:5e:00:01:01",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := GatewayMAC(tc.params)
			if err != nil {
				t.Fatalf("GatewayMAC: %v", err)
			}
			if got.String() != tc.want {
				t.Errorf("GatewayMAC() = %s, want %s", got, tc.want)
			}
		})
	}
}
