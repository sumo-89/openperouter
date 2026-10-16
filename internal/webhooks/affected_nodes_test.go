// SPDX-License-Identifier:Apache-2.0

package webhooks

import (
	"slices"
	"strings"
	"testing"

	"github.com/openperouter/openperouter/api/v1alpha1"
	"github.com/openperouter/openperouter/internal/logging"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestAffectedNodes(t *testing.T) {
	nodeA, nodeB, nodeC := roleNode("a"), roleNode("b"), roleNode("c")
	invalid := &metav1.LabelSelector{MatchLabels: map[string]string{"role": "not a valid value!"}}
	tests := []struct {
		name      string
		selectors []*metav1.LabelSelector
		want      []string
	}{
		{name: "no selector", want: nil},
		{name: "nil selects every node", selectors: []*metav1.LabelSelector{nil}, want: []string{"a", "b", "c"}},
		{name: "one selector", selectors: []*metav1.LabelSelector{roleSelector("a")}, want: []string{"a"}},
		{name: "old and new selectors", selectors: []*metav1.LabelSelector{roleSelector("b"), roleSelector("a")},
			want: []string{"a", "b"}},
		{name: "old selector selects every node",
			selectors: []*metav1.LabelSelector{roleSelector("a"), nil}, want: []string{"a", "b", "c"}},
		{name: "invalid selector", selectors: []*metav1.LabelSelector{invalid}, want: []string{"a", "b", "c"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			useFakeWebhookClient(t, objectsFromResources([]*v1.Node{nodeA, nodeB, nodeC}))

			nodes, err := affectedNodes(t.Context(), tc.selectors...)
			if err != nil {
				t.Fatalf("affectedNodes() error = %v", err)
			}
			var got []string
			for _, node := range nodes {
				got = append(got, node.Name)
			}
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Errorf("affectedNodes() = %v, want %v", got, tc.want)
			}
		})
	}
}

// A conflict that is already stored on a node the write does not touch does
// not refuse the write; one on a node the write moves the object to does.
func TestValidateL2VNIUpdateValidatesOnlyAffectedNodes(t *testing.T) {
	conflictOnC := []*v1alpha1.L2VNI{
		selectingL2VNI("dup1", 300, "c"),
		selectingL2VNI("dup2", 300, "c"),
	}
	tests := []struct {
		name        string
		oldL2VNI    *v1alpha1.L2VNI
		newL2VNI    *v1alpha1.L2VNI
		errorString string
	}{
		{
			name:     "update on a node without conflicts",
			oldL2VNI: selectingL2VNI("l2", 100, "a"),
			newL2VNI: withExportRT(selectingL2VNI("l2", 100, "a"), "65000:100"),
		},
		{
			name:     "move between nodes without conflicts",
			oldL2VNI: selectingL2VNI("l2", 100, "a"),
			newL2VNI: selectingL2VNI("l2", 100, "b"),
		},
		{
			name:        "move to the node with the conflict",
			oldL2VNI:    selectingL2VNI("l2", 100, "a"),
			newL2VNI:    selectingL2VNI("l2", 100, "c"),
			errorString: "duplicate",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			objects := objectsFromResources([]*v1.Node{roleNode("a"), roleNode("b"), roleNode("c")})
			objects = append(objects, objectsFromResources(append(conflictOnC, tc.oldL2VNI))...)
			useFakeWebhookClient(t, objects)

			err := validateL2VNIUpdate(t.Context(), tc.oldL2VNI, tc.newL2VNI)
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

func useFakeWebhookClient(t *testing.T, objects []client.Object) {
	t.Helper()
	fakeClient, err := setupFakeWebhookClient(objects)
	if err != nil {
		t.Fatal(err)
	}
	originalWebhookClient := WebhookClient
	originalLogger := Logger
	t.Cleanup(func() {
		WebhookClient = originalWebhookClient
		Logger = originalLogger
	})
	WebhookClient = fakeClient
	Logger, _ = logging.New("debug")
}

func roleNode(role string) *v1.Node {
	return &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: role, Labels: map[string]string{"role": role}}}
}

func roleSelector(role string) *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: map[string]string{"role": role}}
}

func selectingL2VNI(name string, vni int32, role string) *v1alpha1.L2VNI {
	return &v1alpha1.L2VNI{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
		Spec:       v1alpha1.L2VNISpec{VNI: vni, NodeSelector: roleSelector(role)},
	}
}

func withExportRT(l2 *v1alpha1.L2VNI, rt string) *v1alpha1.L2VNI {
	l2.Spec.ExportRTs = []v1alpha1.RouteTarget{v1alpha1.RouteTarget(rt)}
	return l2
}
