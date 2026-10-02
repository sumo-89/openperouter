// SPDX-License-Identifier:Apache-2.0

package webhooks

import (
	"errors"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/openperouter/openperouter/internal/conversion"
	openpeerrors "github.com/openperouter/openperouter/internal/errors"
)

// overlayWriteError validates the overlay of every node with the written
// object (after) and returns the failures the write is responsible for: those
// that are not there without it (before). That covers the written object
// itself and any object it would take a VNI, a VRF or a subnet from.
//
// A conflict that already exists between other objects is not the writer's
// and does not deny the write. Such a conflict can reach a node through a node
// label, which no admission sees; denying on it would deny every later L2VNI,
// L3VNI and L3VPN write in the cluster until somebody removed it. The node's
// own pass leaves the losing object out and reports it in the node's status.
//
// Failures are told apart by kind and name, as the status reports them.
func overlayWriteError(nodes []v1.Node, before, after conversion.APIConfigData) error {
	afterErr := conversion.ValidateOverlayResourcesForNodes(nodes, after.L2VNIs, after.L3VNIs, after.L3VPNs)
	if afterErr == nil {
		return nil
	}
	beforeErr := conversion.ValidateOverlayResourcesForNodes(nodes, before.L2VNIs, before.L3VNIs, before.L3VPNs)
	failedBefore := sets.New[string]()
	for _, failure := range openpeerrors.CollectFailures(beforeErr) {
		failedBefore.Insert(string(failure.Kind) + "/" + failure.Name)
	}

	var caused []error
	for _, err := range joinedErrors(afterErr) {
		var resourceErr *openpeerrors.ResourceError
		if errors.As(err, &resourceErr) && failedBefore.Has(string(resourceErr.Obj.Kind)+"/"+resourceErr.Obj.Name) {
			continue
		}
		caused = append(caused, err)
	}
	return errors.Join(caused...)
}

func joinedErrors(err error) []error {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		return joined.Unwrap()
	}
	return []error{err}
}
