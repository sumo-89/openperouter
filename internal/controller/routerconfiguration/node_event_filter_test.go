// SPDX-License-Identifier:Apache-2.0

package routerconfiguration

import (
	"errors"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/openperouter/openperouter/api/v1alpha1"
)

func TestNodeScopedEventFilter(t *testing.T) {
	nodeA := &v1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "node-a", Labels: map[string]string{"kubernetes.io/hostname": "node-a"},
	}}
	onA := func() (*v1.Node, error) { return nodeA, nil }

	tests := []struct {
		name    string
		getNode func() (*v1.Node, error)
		obj     client.Object
		want    bool
	}{
		{name: "L2VNI selecting this node", getNode: onA, obj: l2vniFor(selectHost("node-a")), want: true},
		{name: "L2VNI selecting another node", getNode: onA, obj: l2vniFor(selectHost("node-b")), want: false},
		{name: "L2VNI without a selector", getNode: onA, obj: l2vniFor(nil), want: true},
		{name: "Underlay for another node", getNode: onA, obj: &v1alpha1.Underlay{
			Spec: v1alpha1.UnderlaySpec{NodeSelector: selectHost("node-b")}}, want: false},
		{name: "L3VNI for another node", getNode: onA, obj: &v1alpha1.L3VNI{
			Spec: v1alpha1.L3VNISpec{NodeSelector: selectHost("node-b")}}, want: false},
		{name: "L3VPN for another node", getNode: onA, obj: &v1alpha1.L3VPN{
			Spec: v1alpha1.L3VPNSpec{NodeSelector: selectHost("node-b")}}, want: false},
		{name: "L3Passthrough for another node", getNode: onA, obj: &v1alpha1.L3Passthrough{
			Spec: v1alpha1.L3PassthroughSpec{NodeSelector: selectHost("node-b")}}, want: false},
		{name: "RawFRRConfig for another node", getNode: onA, obj: &v1alpha1.RawFRRConfig{
			Spec: v1alpha1.RawFRRConfigSpec{NodeSelector: selectHost("node-b")}}, want: false},
		{name: "objects without a node selector pass", getNode: onA, obj: &v1.Secret{}, want: true},
		{name: "an unreadable node lets the event through",
			getNode: func() (*v1.Node, error) { return nil, errors.New("not in cache") },
			obj:     l2vniFor(selectHost("node-b")), want: true},
		{name: "an unparsable selector lets the event through", getNode: onA,
			obj: l2vniFor(&metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "kubernetes.io/hostname", Operator: metav1.LabelSelectorOpIn},
			}}), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := nodeScopedEventFilter(tt.getNode)
			if got := f.Create(event.CreateEvent{Object: tt.obj}); got != tt.want {
				t.Errorf("create: got %v, want %v", got, tt.want)
			}
			if got := f.Delete(event.DeleteEvent{Object: tt.obj}); got != tt.want {
				t.Errorf("delete: got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNodeScopedEventFilterUpdateMatchesEitherVersion(t *testing.T) {
	nodeA := &v1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "node-a", Labels: map[string]string{"kubernetes.io/hostname": "node-a"},
	}}
	f := nodeScopedEventFilter(func() (*v1.Node, error) { return nodeA, nil })

	tests := []struct {
		name     string
		old, new *metav1.LabelSelector
		want     bool
	}{
		{name: "moves onto this node", old: selectHost("node-b"), new: selectHost("node-a", "node-b"), want: true},
		{name: "moves away from this node", old: selectHost("node-a"), new: selectHost("node-b"), want: true},
		{name: "never on this node", old: selectHost("node-b"), new: selectHost("node-b", "node-c"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := event.UpdateEvent{ObjectOld: l2vniFor(tt.old), ObjectNew: l2vniFor(tt.new)}
			if got := f.Update(e); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func l2vniFor(sel *metav1.LabelSelector) *v1alpha1.L2VNI {
	return &v1alpha1.L2VNI{Spec: v1alpha1.L2VNISpec{NodeSelector: sel}}
}

func selectHost(hosts ...string) *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
		{Key: "kubernetes.io/hostname", Operator: metav1.LabelSelectorOpIn, Values: hosts},
	}}
}
