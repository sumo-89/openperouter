// SPDX-License-Identifier:Apache-2.0

package routerconfiguration

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openperouter/openperouter/api/v1alpha1"
)

func TestGetConfigFromAPISkipsUnparsableSelectors(t *testing.T) {
	broken := &v1alpha1.L2VNI{
		ObjectMeta: metav1.ObjectMeta{Name: "broken", Namespace: testNamespace},
		Spec: v1alpha1.L2VNISpec{VNI: 100, NodeSelector: &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "kubernetes.io/hostname", Operator: metav1.LabelSelectorOpIn},
			},
		}},
	}
	fine := &v1alpha1.L2VNI{
		ObjectMeta: metav1.ObjectMeta{Name: "fine", Namespace: testNamespace},
		Spec:       v1alpha1.L2VNISpec{VNI: 101},
	}
	r := newTestPERouterReconciler(t, &noopDatapathConfigurator{}, testNode(), broken, fine)

	config, err := r.getConfigFromAPI(context.Background(), testLogger())
	if err != nil {
		t.Fatalf("an unparsable selector must not fail the node's reconcile: %v", err)
	}
	if len(config.L2VNIs) != 1 || config.L2VNIs[0].Name != "fine" {
		t.Fatalf("expected only the L2VNI with a parsable selector, got %v", config.L2VNIs)
	}
}
