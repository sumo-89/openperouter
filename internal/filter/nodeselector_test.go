// SPDX-License-Identifier:Apache-2.0

package filter_test

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openperouter/openperouter/api/v1alpha1"
	"github.com/openperouter/openperouter/internal/filter"
)

func TestFilterRawFRRConfigsForNode(t *testing.T) {
	tests := []struct {
		name          string
		nodeLabels    map[string]string
		rawConfigs    []v1alpha1.RawFRRConfig
		expectedCount int
		expectedNames []string
	}{
		{
			name:       "nil selector matches all",
			nodeLabels: map[string]string{"rack": "rack-1"},
			rawConfigs: []v1alpha1.RawFRRConfig{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "raw-all"},
					Spec:       v1alpha1.RawFRRConfigSpec{RawConfig: "test config"},
				},
			},
			expectedCount: 1,
			expectedNames: []string{"raw-all"},
		},
		{
			name:       "matching selector",
			nodeLabels: map[string]string{"kubernetes.io/hostname": "node-1"},
			rawConfigs: []v1alpha1.RawFRRConfig{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "raw-node-1"},
					Spec: v1alpha1.RawFRRConfigSpec{
						NodeSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"kubernetes.io/hostname": "node-1"},
						},
						RawConfig: "test config",
					},
				},
			},
			expectedCount: 1,
			expectedNames: []string{"raw-node-1"},
		},
		{
			name:       "non-matching selector",
			nodeLabels: map[string]string{"kubernetes.io/hostname": "node-1"},
			rawConfigs: []v1alpha1.RawFRRConfig{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "raw-node-2"},
					Spec: v1alpha1.RawFRRConfigSpec{
						NodeSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"kubernetes.io/hostname": "node-2"},
						},
						RawConfig: "test config",
					},
				},
			},
			expectedCount: 0,
			expectedNames: []string{},
		},
		{
			name:       "mixed selectors filter correctly",
			nodeLabels: map[string]string{"rack": "rack-1"},
			rawConfigs: []v1alpha1.RawFRRConfig{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "raw-matching"},
					Spec: v1alpha1.RawFRRConfigSpec{
						NodeSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"rack": "rack-1"},
						},
						RawConfig: "matching config",
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "raw-non-matching"},
					Spec: v1alpha1.RawFRRConfigSpec{
						NodeSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"rack": "rack-2"},
						},
						RawConfig: "non-matching config",
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "raw-all-nodes"},
					Spec:       v1alpha1.RawFRRConfigSpec{RawConfig: "all nodes config"},
				},
			},
			expectedCount: 2,
			expectedNames: []string{"raw-matching", "raw-all-nodes"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Labels: tt.nodeLabels,
				},
			}

			filtered, err := filter.RawFRRConfigsForNode(node, tt.rawConfigs)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if len(filtered) != tt.expectedCount {
				t.Errorf("expected %d raw configs, got %d", tt.expectedCount, len(filtered))
			}

			for i, expectedName := range tt.expectedNames {
				if i >= len(filtered) {
					t.Errorf("missing expected raw config: %s", expectedName)
					continue
				}
				if filtered[i].Name != expectedName {
					t.Errorf("expected raw config name %s, got %s", expectedName, filtered[i].Name)
				}
			}
		})
	}
}

func TestFilterUnderlaysForNode(t *testing.T) {
	tests := []struct {
		name          string
		nodeLabels    map[string]string
		underlays     []v1alpha1.Underlay
		expectedCount int
		expectedNames []string
	}{
		{
			name:       "no selector matches all",
			nodeLabels: map[string]string{"rack": "rack-1"},
			underlays: []v1alpha1.Underlay{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "underlay-1"},
					Spec:       v1alpha1.UnderlaySpec{NodeSelector: nil},
				},
			},
			expectedCount: 1,
			expectedNames: []string{"underlay-1"},
		},
		{
			name:       "matching selector",
			nodeLabels: map[string]string{"rack": "rack-1"},
			underlays: []v1alpha1.Underlay{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "underlay-rack-1"},
					Spec: v1alpha1.UnderlaySpec{
						NodeSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"rack": "rack-1"},
						},
					},
				},
			},
			expectedCount: 1,
			expectedNames: []string{"underlay-rack-1"},
		},
		{
			name:       "non-matching selector",
			nodeLabels: map[string]string{"rack": "rack-1"},
			underlays: []v1alpha1.Underlay{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "underlay-rack-2"},
					Spec: v1alpha1.UnderlaySpec{
						NodeSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"rack": "rack-2"},
						},
					},
				},
			},
			expectedCount: 0,
			expectedNames: []string{},
		},
		{
			name:       "multiple underlays with different selectors",
			nodeLabels: map[string]string{"rack": "rack-1", "zone": "us-east-1a"},
			underlays: []v1alpha1.Underlay{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "underlay-rack-1"},
					Spec: v1alpha1.UnderlaySpec{
						NodeSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"rack": "rack-1"},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "underlay-rack-2"},
					Spec: v1alpha1.UnderlaySpec{
						NodeSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"rack": "rack-2"},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "underlay-all"},
					Spec:       v1alpha1.UnderlaySpec{NodeSelector: nil},
				},
			},
			expectedCount: 2,
			expectedNames: []string{"underlay-rack-1", "underlay-all"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Labels: tt.nodeLabels,
				},
			}

			filtered, err := filter.UnderlaysForNode(node, tt.underlays)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if len(filtered) != tt.expectedCount {
				t.Errorf("expected %d underlays, got %d", tt.expectedCount, len(filtered))
			}

			for i, expectedName := range tt.expectedNames {
				if i >= len(filtered) {
					t.Errorf("missing expected underlay: %s", expectedName)
					continue
				}
				if filtered[i].Name != expectedName {
					t.Errorf("expected underlay name %s, got %s", expectedName, filtered[i].Name)
				}
			}
		})
	}
}

func TestDropUnparsableSelectors(t *testing.T) {
	emptyIn := &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
		{Key: "kubernetes.io/hostname", Operator: metav1.LabelSelectorOpIn},
	}}
	items := []v1alpha1.L2VNI{
		{ObjectMeta: metav1.ObjectMeta{Name: "no-selector"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "broken"}, Spec: v1alpha1.L2VNISpec{NodeSelector: emptyIn}},
		{ObjectMeta: metav1.ObjectMeta{Name: "fine"}, Spec: v1alpha1.L2VNISpec{NodeSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"kubernetes.io/hostname": "node-1"},
		}}},
	}

	valid, err := filter.DropUnparsableSelectors(items,
		func(v v1alpha1.L2VNI) *metav1.LabelSelector { return v.Spec.NodeSelector },
		func(v v1alpha1.L2VNI) string { return v.Name })

	if err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("expected an error naming the broken item, got %v", err)
	}
	if len(valid) != 2 || valid[0].Name != "no-selector" || valid[1].Name != "fine" {
		t.Fatalf("expected the two parsable items in order, got %v", valid)
	}

	// Admission relies on the ForNode filters failing on such a selector.
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"kubernetes.io/hostname": "node-1"}}}
	if _, err := filter.L2VNIsForNode(node, items); err == nil {
		t.Fatal("L2VNIsForNode must still fail on an unparsable selector")
	}
}
