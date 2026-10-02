// SPDX-License-Identifier:Apache-2.0

package routerconfiguration

import (
	"context"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/openperouter/openperouter/api/v1alpha1"
	"github.com/openperouter/openperouter/internal/conversion"
	openpeerrors "github.com/openperouter/openperouter/internal/errors"
)

// The tests in this file pin fault isolation in a pass: an object that
// conflicts with another one on the node is left out and reported on its own,
// the object that was there first keeps its configuration, and no other object
// of the VRF or the node is touched.

var isolationEpoch = time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)

// isolationL3 is the L3VNI, of VRF vrfB, that the L2VNIs of these tests bind.
const isolationL3 = "vrf-b"

// createdAt sets the creation timestamp of an object to the epoch plus the
// given number of seconds.
func createdAt[T interface{ SetCreationTimestamp(metav1.Time) }](obj T, seconds int) T {
	obj.SetCreationTimestamp(metav1.NewTime(isolationEpoch.Add(time.Duration(seconds) * time.Second)))
	return obj
}

func agedL3VNI(name, vrf string, vni int32, seconds int) v1alpha1.L3VNI {
	l3 := l3VNI(name, vrf, vni)
	return *createdAt(&l3, seconds)
}

func agedL2VNI(name, l3 string, vni int32, gateway string, seconds int) v1alpha1.L2VNI {
	l2 := l2VNIWithGateway(name, l3vniRoutingDomain(l3), vni, []string{gateway})
	return *createdAt(&l2, seconds)
}

func reconcileForIsolation(t *testing.T, config conversion.APIConfigData) (conversion.APIConfigData, []v1alpha1.FailedResource) {
	t.Helper()
	var got conversion.APIConfigData
	configure := func(_ context.Context, data frrConfigData) error {
		got = data.APIConfigData
		return nil
	}
	err := Reconcile(context.Background(), config, 0, "", "", "", noopUpdater, &noopDatapathConfigurator{}, configure)
	if openpeerrors.IsNonResourceError(err) {
		t.Fatalf("pass failed as a whole: %v", err)
	}
	return got, openpeerrors.CollectFailures(err)
}

func failedNames(failures []v1alpha1.FailedResource) []string {
	names := make([]string, 0, len(failures))
	for _, f := range failures {
		names = append(names, string(f.Kind)+"/"+f.Name)
	}
	return names
}

func l2Names(l2s []v1alpha1.L2VNI) []string {
	names := make([]string, 0, len(l2s))
	for _, l2 := range l2s {
		names = append(names, l2.Name)
	}
	return names
}

// T16 (a) in the kind lab: one L2VNI whose subnet overlaps another of the same
// VRF took the L3VNI and every L2VNI of that VRF off the node.
func TestReconcileSubnetOverlapLeavesOutOnlyTheNewcomer(t *testing.T) {
	config := conversion.APIConfigData{
		L3VNIs: []v1alpha1.L3VNI{agedL3VNI(isolationL3, "vrfB", 10001, 0)},
		L2VNIs: []v1alpha1.L2VNI{
			agedL2VNI("t1-net-1", isolationL3, 30201, "10.231.1.1/24", 10),
			agedL2VNI("t1-net-2", isolationL3, 30202, "10.231.2.1/24", 20),
			// Sorts first by name, created last.
			agedL2VNI("a-overlap", isolationL3, 30299, "10.231.1.129/25", 30),
		},
	}

	got, failures := reconcileForIsolation(t, config)

	if diff := cmp.Diff([]string{"L2VNI/a-overlap"}, failedNames(failures)); diff != "" {
		t.Errorf("failed resources (-want +got):\n%s", diff)
	}
	if len(got.L3VNIs) != 1 || got.L3VNIs[0].Name != isolationL3 {
		t.Errorf("the L3VNI of the VRF must stay, got %v", got.L3VNIs)
	}
	if diff := cmp.Diff([]string{"t1-net-1", "t1-net-2"}, l2Names(got.L2VNIs)); diff != "" {
		t.Errorf("L2VNIs configured (-want +got):\n%s", diff)
	}
	if len(failures) == 1 {
		want := `subnet overlap in VRF "vrfB": IPNet 10.231.1.128/25 (L2VNI /a-overlap) overlaps with IPNet 10.231.1.0/24 (L2VNI /t1-net-1)`
		if failures[0].Message != want {
			t.Errorf("message:\n got  %q\n want %q", failures[0].Message, want)
		}
	}
}

// An L2VNI that overlaps the host session of its own L3VNI is the one left out,
// whatever the ages: leaving out the L3VNI would take the whole VRF with it.
func TestReconcileSubnetOverlapWithTheL3VNIKeepsTheVRF(t *testing.T) {
	l3 := agedL3VNI(isolationL3, "vrfB", 10001, 30)
	l3.Spec.HostSession = &v1alpha1.HostSession{ASN: 64514, HostASN: ptr.To(int64(64515)), LocalCIDRs: []string{"192.0.2.0/24"}}
	config := conversion.APIConfigData{
		L3VNIs: []v1alpha1.L3VNI{l3},
		L2VNIs: []v1alpha1.L2VNI{
			agedL2VNI("old-overlap", isolationL3, 30201, "192.0.2.65/26", 0),
			agedL2VNI("t1-net-2", isolationL3, 30202, "10.231.2.1/24", 10),
		},
	}

	got, failures := reconcileForIsolation(t, config)

	if diff := cmp.Diff([]string{"L2VNI/old-overlap"}, failedNames(failures)); diff != "" {
		t.Errorf("failed resources (-want +got):\n%s", diff)
	}
	if len(got.L3VNIs) != 1 {
		t.Errorf("the L3VNI of the VRF must stay, got %v", got.L3VNIs)
	}
	if diff := cmp.Diff([]string{"t1-net-2"}, l2Names(got.L2VNIs)); diff != "" {
		t.Errorf("L2VNIs configured (-want +got):\n%s", diff)
	}
}

// An L2VNI with an IPv4 and an IPv6 gateway is one object: if either family
// overlaps, the whole L2VNI is left out, and its other family then blocks
// nothing.
func TestReconcileSubnetOverlapInOneFamilyLeavesOutTheObject(t *testing.T) {
	dual := agedL2VNI("dual", isolationL3, 30203, "10.231.3.1/24", 20)
	dual.Spec.GatewayIPs = append(dual.Spec.GatewayIPs, "fd00:1::1/64")
	v6 := agedL2VNI("v6-old", isolationL3, 30204, "fd00:1::2/64", 10)
	v6.Spec.GatewayIPs = []string{"fd00:1::2/64"}
	later := agedL2VNI("v4-later", isolationL3, 30205, "10.231.3.2/24", 30)
	config := conversion.APIConfigData{
		L3VNIs: []v1alpha1.L3VNI{agedL3VNI(isolationL3, "vrfB", 10001, 0)},
		L2VNIs: []v1alpha1.L2VNI{dual, v6, later},
	}

	got, failures := reconcileForIsolation(t, config)

	if diff := cmp.Diff([]string{"L2VNI/dual"}, failedNames(failures)); diff != "" {
		t.Errorf("failed resources (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"v4-later", "v6-old"}, l2Names(got.L2VNIs)); diff != "" {
		t.Errorf("L2VNIs configured (-want +got):\n%s", diff)
	}
}

// T16 (b): on a duplicate VNI the object that sorts first by name won, so a
// new object could take the VNI from the one in service.
func TestReconcileDuplicateVNIKeepsTheOlderObject(t *testing.T) {
	config := conversion.APIConfigData{
		L3VNIs: []v1alpha1.L3VNI{agedL3VNI(isolationL3, "vrfB", 10001, 0)},
		L2VNIs: []v1alpha1.L2VNI{
			agedL2VNI("t1-net-1", isolationL3, 30201, "10.231.1.1/24", 10),
			agedL2VNI("a-dupvni", isolationL3, 30201, "10.231.9.1/24", 30),
		},
	}

	got, failures := reconcileForIsolation(t, config)

	if diff := cmp.Diff([]string{"L2VNI/a-dupvni"}, failedNames(failures)); diff != "" {
		t.Errorf("failed resources (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"t1-net-1"}, l2Names(got.L2VNIs)); diff != "" {
		t.Errorf("L2VNIs configured (-want +got):\n%s", diff)
	}
}

// The same for an L3VNI: a new L3VNI with the VNI or the VRF of one in service
// is left out, the one in service stays.
func TestReconcileDuplicateL3VNIKeepsTheOlderObject(t *testing.T) {
	config := conversion.APIConfigData{
		L3VNIs: []v1alpha1.L3VNI{
			agedL3VNI(isolationL3, "vrfB", 10001, 10),
			agedL3VNI("a-dup-vni", "vrfC", 10001, 20),
			agedL3VNI("a-dup-vrf", "vrfB", 10002, 30),
		},
	}

	got, failures := reconcileForIsolation(t, config)

	if diff := cmp.Diff([]string{"L3VNI/a-dup-vni", "L3VNI/a-dup-vrf"}, failedNames(failures)); diff != "" {
		t.Errorf("failed resources (-want +got):\n%s", diff)
	}
	if len(got.L3VNIs) != 1 || got.L3VNIs[0].Name != isolationL3 {
		t.Errorf("L3VNIs configured: %v", got.L3VNIs)
	}
}

// Ages decide only conflicts. A valid configuration reaches FRR and the
// datapath in name order, as before, whatever order the objects were created
// in, so the rendered configuration does not change.
func TestReconcileValidConfigKeepsNameOrder(t *testing.T) {
	config := conversion.APIConfigData{
		L3VNIs: []v1alpha1.L3VNI{
			agedL3VNI("vrf-c", "vrfC", 10002, 0),
			agedL3VNI(isolationL3, "vrfB", 10001, 10),
		},
		L2VNIs: []v1alpha1.L2VNI{
			agedL2VNI("z-net", isolationL3, 30203, "10.231.3.1/24", 0),
			agedL2VNI("m-net", "vrf-c", 30202, "10.231.2.1/24", 10),
			agedL2VNI("a-net", isolationL3, 30201, "10.231.1.1/24", 20),
		},
	}

	got, failures := reconcileForIsolation(t, config)

	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %v", failures)
	}
	if diff := cmp.Diff([]string{"a-net", "m-net", "z-net"}, l2Names(got.L2VNIs)); diff != "" {
		t.Errorf("L2VNI order (-want +got):\n%s", diff)
	}
	if got.L3VNIs[0].Name != isolationL3 || got.L3VNIs[1].Name != "vrf-c" {
		t.Errorf("L3VNI order: %v", got.L3VNIs)
	}
}

// Case (e) at render time: an L2VNI with a route target FRR cannot take (here
// with a "target:" prefix) is left out on its own.
func TestReconcileBadRouteTargetLeavesOutOnlyThatL2VNI(t *testing.T) {
	bad := agedL2VNI("a-badrt", isolationL3, 30299, "10.231.9.1/24", 0)
	bad.Spec.ExportRTs = []v1alpha1.RouteTarget{"target:65000:30299"}
	config := conversion.APIConfigData{
		L3VNIs: []v1alpha1.L3VNI{agedL3VNI(isolationL3, "vrfB", 10001, 0)},
		L2VNIs: []v1alpha1.L2VNI{bad, agedL2VNI("t1-net-1", isolationL3, 30201, "10.231.1.1/24", 10)},
	}

	got, failures := reconcileForIsolation(t, config)

	if diff := cmp.Diff([]string{"L2VNI/a-badrt"}, failedNames(failures)); diff != "" {
		t.Errorf("failed resources (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"t1-net-1"}, l2Names(got.L2VNIs)); diff != "" {
		t.Errorf("L2VNIs configured (-want +got):\n%s", diff)
	}
}
