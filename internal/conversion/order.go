// SPDX-License-Identifier:Apache-2.0

package conversion

import (
	"cmp"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// OrderOldestFirst sorts the L3VNIs, L3VPNs, L2VNIs and L3Passthroughs of the
// configuration oldest first: by creation timestamp, then by namespace/name.
//
// The filters that resolve a conflict between two objects (a duplicate VNI or
// VRF, an overlapping subnet) keep the object they meet first and leave out
// the other. In this order the object that was there first wins, so a new
// object that conflicts with one in service is the one left out, whatever the
// two are called. An object without a creation timestamp, such as a static
// configuration, counts as the newest.
//
// The order decides conflicts only. Callers that render the configuration
// sort it by name again afterwards, so a valid configuration renders the same
// whatever order its objects were created in.
func OrderOldestFirst(config *APIConfigData) {
	oldestFirst(config.L3VNIs)
	oldestFirst(config.L3VPNs)
	oldestFirst(config.L2VNIs)
	oldestFirst(config.L3Passthrough)
}

func oldestFirst[T any, PT interface {
	*T
	metav1.Object
}](items []T) {
	slices.SortStableFunc(items, func(a, b T) int {
		return compareAge(PT(&a), PT(&b))
	})
}

func compareAge(a, b metav1.Object) int {
	ta, tb := a.GetCreationTimestamp(), b.GetCreationTimestamp()
	switch {
	case ta.IsZero() && !tb.IsZero():
		return 1
	case !ta.IsZero() && tb.IsZero():
		return -1
	}
	if c := ta.Compare(tb.Time); c != 0 {
		return c
	}
	return cmp.Compare(
		types.NamespacedName{Namespace: a.GetNamespace(), Name: a.GetName()}.String(),
		types.NamespacedName{Namespace: b.GetNamespace(), Name: b.GetName()}.String())
}
