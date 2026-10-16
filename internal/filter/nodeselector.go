// SPDX-License-Identifier:Apache-2.0

package filter

import (
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/openperouter/openperouter/api/v1alpha1"
)

// UnderlaysForNode returns underlays that match the given node's labels.
func UnderlaysForNode(node *corev1.Node, underlays []v1alpha1.Underlay) ([]v1alpha1.Underlay, error) {
	return filterForNode(node, underlays, func(u v1alpha1.Underlay) *metav1.LabelSelector {
		return u.Spec.NodeSelector
	})
}

// L3VNIsForNode returns L3VNIs that match the given node's labels.
func L3VNIsForNode(node *corev1.Node, l3vnis []v1alpha1.L3VNI) ([]v1alpha1.L3VNI, error) {
	return filterForNode(node, l3vnis, func(v v1alpha1.L3VNI) *metav1.LabelSelector {
		return v.Spec.NodeSelector
	})
}

// L2VNIsForNode returns L2VNIs that match the given node's labels.
func L2VNIsForNode(node *corev1.Node, l2vnis []v1alpha1.L2VNI) ([]v1alpha1.L2VNI, error) {
	return filterForNode(node, l2vnis, func(v v1alpha1.L2VNI) *metav1.LabelSelector {
		return v.Spec.NodeSelector
	})
}

// L3VPNsForNode returns L3VPNs that match the given node's labels.
func L3VPNsForNode(node *corev1.Node, l3vpns []v1alpha1.L3VPN) ([]v1alpha1.L3VPN, error) {
	return filterForNode(node, l3vpns, func(v v1alpha1.L3VPN) *metav1.LabelSelector {
		return v.Spec.NodeSelector
	})
}

// L3PassthroughsForNode returns L3Passthroughs that match the given node's labels.
func L3PassthroughsForNode(node *corev1.Node, l3passthroughs []v1alpha1.L3Passthrough) ([]v1alpha1.L3Passthrough, error) {
	return filterForNode(node, l3passthroughs, func(p v1alpha1.L3Passthrough) *metav1.LabelSelector {
		return p.Spec.NodeSelector
	})
}

// RawFRRConfigsForNode returns RawFRRConfigs that match the given node's labels.
func RawFRRConfigsForNode(node *corev1.Node, rawFRRConfigs []v1alpha1.RawFRRConfig) ([]v1alpha1.RawFRRConfig, error) {
	return filterForNode(node, rawFRRConfigs, func(r v1alpha1.RawFRRConfig) *metav1.LabelSelector {
		return r.Spec.NodeSelector
	})
}

// DropUnparsableSelectors returns the items whose nodeSelector can be parsed,
// and an error naming each item whose selector cannot. The ForNode filters
// fail as a whole on such an item, which is what admission wants; a
// reconcile wants to go on with the other items instead.
func DropUnparsableSelectors[T any](items []T, getSelector func(T) *metav1.LabelSelector,
	getName func(T) string) ([]T, error) {
	valid := make([]T, 0, len(items))
	var errs []error
	for _, item := range items {
		sel := getSelector(item)
		if sel == nil {
			valid = append(valid, item)
			continue
		}
		if _, err := metav1.LabelSelectorAsSelector(sel); err != nil {
			errs = append(errs, fmt.Errorf("%s: invalid nodeSelector: %w", getName(item), err))
			continue
		}
		valid = append(valid, item)
	}
	return valid, errors.Join(errs...)
}

// filterForNode is a generic function that filters items based on node label selectors.
// It takes a selector function that extracts the NodeSelector from each item.
func filterForNode[T any](node *corev1.Node, items []T, getSelector func(T) *metav1.LabelSelector) ([]T, error) {
	var result []T
	for _, item := range items {
		nodeSelector := getSelector(item)

		// nil NodeSelector matches all nodes
		if nodeSelector == nil {
			result = append(result, item)
			continue
		}

		labelSelector, err := metav1.LabelSelectorAsSelector(nodeSelector)
		if err != nil {
			return nil, err
		}

		if labelSelector.Matches(labels.Set(node.Labels)) {
			result = append(result, item)
		}
	}
	return result, nil
}
