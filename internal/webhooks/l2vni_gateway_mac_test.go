// SPDX-License-Identifier:Apache-2.0

package webhooks

import (
	"strings"
	"testing"

	"github.com/openperouter/openperouter/api/v1alpha1"
	"github.com/openperouter/openperouter/internal/logging"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const anycastGatewayMAC = "00:00:5e:00:01:01"

func TestValidateL2VNIGatewayMAC(t *testing.T) {
	tcs := []struct {
		name        string
		stored      []*v1alpha1.L2VNI
		oldL2VNI    *v1alpha1.L2VNI
		newL2VNI    *v1alpha1.L2VNI
		errorString string
	}{
		{
			name:     "create with a gatewayMAC",
			newL2VNI: gatewayMACL2VNI("l2a", 100, anycastGatewayMAC),
		},
		{
			name:     "create with the gatewayMAC of a stored L2VNI",
			stored:   []*v1alpha1.L2VNI{gatewayMACL2VNI("l2a", 100, anycastGatewayMAC)},
			newL2VNI: gatewayMACL2VNI("l2b", 101, anycastGatewayMAC),
		},
		{
			name:        "create with a multicast gatewayMAC",
			newL2VNI:    gatewayMACL2VNI("l2a", 100, "01:00:5e:00:00:01"),
			errorString: "must be unicast",
		},
		{
			name:        "create with a malformed gatewayMAC",
			newL2VNI:    gatewayMACL2VNI("l2a", 100, "00-00-5e-00-01-01"),
			errorString: "48-bit MAC in colon form",
		},
		{
			name:        "create with a gatewayMAC and no gatewayIPs",
			newL2VNI:    withoutGatewayIPs(gatewayMACL2VNI("l2a", 100, anycastGatewayMAC)),
			errorString: "gatewayMAC requires gatewayIPs",
		},
		{
			name:     "update that keeps the gatewayMAC",
			oldL2VNI: gatewayMACL2VNI("l2a", 100, anycastGatewayMAC),
			newL2VNI: gatewayMACL2VNI("l2a", 100, anycastGatewayMAC),
		},
		{
			name:        "update that changes the gatewayMAC",
			oldL2VNI:    gatewayMACL2VNI("l2a", 100, anycastGatewayMAC),
			newL2VNI:    gatewayMACL2VNI("l2a", 100, "00:00:5e:00:01:02"),
			errorString: "gatewayMAC cannot be changed",
		},
		{
			name:        "update that adds a gatewayMAC",
			oldL2VNI:    gatewayMACL2VNI("l2a", 100, ""),
			newL2VNI:    gatewayMACL2VNI("l2a", 100, anycastGatewayMAC),
			errorString: "gatewayMAC cannot be changed",
		},
		{
			name:        "update that removes the gatewayMAC",
			oldL2VNI:    gatewayMACL2VNI("l2a", 100, anycastGatewayMAC),
			newL2VNI:    gatewayMACL2VNI("l2a", 100, ""),
			errorString: "gatewayMAC cannot be changed",
		},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			node := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1"}}
			objects := append(objectsFromResources(tc.stored), objectsFromResources([]*v1.Node{node})...)
			client, err := setupFakeWebhookClient(objects)
			if err != nil {
				t.Fatal(err)
			}
			origWebhookClient := WebhookClient
			origLogger := Logger
			defer func() {
				WebhookClient = origWebhookClient
				Logger = origLogger
			}()
			WebhookClient = client
			Logger, _ = logging.New("debug")

			if tc.oldL2VNI == nil {
				err = validateL2VNICreate(tc.newL2VNI)
			} else {
				err = validateL2VNIUpdate(tc.oldL2VNI, tc.newL2VNI)
			}
			if tc.errorString == "" {
				if err != nil {
					t.Fatalf("expected no error, but got %q", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.errorString) {
				t.Fatalf("expected an error containing %q, got %v", tc.errorString, err)
			}
		})
	}
}

func gatewayMACL2VNI(name string, vni int32, mac string) *v1alpha1.L2VNI {
	l2 := &v1alpha1.L2VNI{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
		Spec: v1alpha1.L2VNISpec{
			VNI:        vni,
			GatewayIPs: []string{"192.0.2.1/24"},
		},
	}
	if mac != "" {
		l2.Spec.GatewayMAC = &mac
	}
	return l2
}

func withoutGatewayIPs(l2 *v1alpha1.L2VNI) *v1alpha1.L2VNI {
	l2.Spec.GatewayIPs = nil
	return l2
}
