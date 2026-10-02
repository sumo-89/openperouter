// SPDX-License-Identifier:Apache-2.0

package webhooks

import (
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openperouter/openperouter/api/v1alpha1"
	"github.com/openperouter/openperouter/internal/logging"
)

// The tests in this file pin fault isolation in admission: a write is denied
// for what the written object does, on its own or to others, and never for a
// conflict that already exists between other objects. Without that, one
// conflict that reaches a node by a label change (no admission sees it) denies
// every later L2VNI, L3VNI and L3VPN write in the cluster until it is removed.

var admissionEpoch = time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)

const (
	isolationNS = "openperouter-system"
	// storedL3 is the L3VNI, of VRF vrfB, that the L2VNIs of these tests bind.
	storedL3 = "vrf-b"
)

func selectingPE() *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: map[string]string{"pe": "true"}}
}

func peNode() *v1.Node {
	return &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", Labels: map[string]string{"pe": "true"}}}
}

func isolationL3VNI(name, vrf string, vni int32) *v1alpha1.L3VNI {
	return &v1alpha1.L3VNI{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: isolationNS, Name: name,
			CreationTimestamp: metav1.NewTime(admissionEpoch),
		},
		Spec: v1alpha1.L3VNISpec{VRF: vrf, VNI: vni, NodeSelector: selectingPE()},
	}
}

func isolationL2VNI(name string, vni int32, gateway string, seconds int) *v1alpha1.L2VNI {
	l2 := &v1alpha1.L2VNI{
		ObjectMeta: metav1.ObjectMeta{Namespace: isolationNS, Name: name},
		Spec: v1alpha1.L2VNISpec{
			VNI:          vni,
			NodeSelector: selectingPE(),
			GatewayIPs:   []string{gateway},
			RoutingDomain: &v1alpha1.RoutingDomain{
				Type:  v1alpha1.RoutingDomainTypeL3VNI,
				L3VNI: &v1alpha1.L3VNIReference{Name: storedL3},
			},
		},
	}
	if seconds >= 0 {
		l2.CreationTimestamp = metav1.NewTime(admissionEpoch.Add(time.Duration(seconds) * time.Second))
	}
	return l2
}

func withExportRT(l2 *v1alpha1.L2VNI, rt string) *v1alpha1.L2VNI {
	l2.Spec.ExportRTs = []v1alpha1.RouteTarget{v1alpha1.RouteTarget(rt)}
	return l2
}

func withSelector(l2 *v1alpha1.L2VNI, sel *metav1.LabelSelector) *v1alpha1.L2VNI {
	l2.Spec.NodeSelector = sel
	return l2
}

// useStoredObjects points the webhook at a fake API server holding the objects.
func useStoredObjects(t *testing.T, nodes []*v1.Node, l3vnis []*v1alpha1.L3VNI, l2vnis []*v1alpha1.L2VNI) {
	t.Helper()
	objects := objectsFromResources(nodes)
	objects = append(objects, objectsFromResources(l3vnis)...)
	objects = append(objects, objectsFromResources(l2vnis)...)
	c, err := setupFakeWebhookClient(objects)
	if err != nil {
		t.Fatal(err)
	}
	origClient, origLogger := WebhookClient, Logger
	t.Cleanup(func() { WebhookClient, Logger = origClient, origLogger })
	WebhookClient = c
	Logger, _ = logging.New("debug")
}

// Conflicts that exist between two stored objects on the node the label
// brought them together on.
var storedConflicts = []struct {
	name   string
	l2vnis []*v1alpha1.L2VNI
}{
	{
		name: "subnet overlap in the VRF",
		l2vnis: []*v1alpha1.L2VNI{
			isolationL2VNI("t1-net-1", 30201, "10.231.1.1/24", 10),
			isolationL2VNI("a-overlap", 30299, "10.231.1.129/25", 20),
		},
	},
	{
		name: "duplicate VNI",
		l2vnis: []*v1alpha1.L2VNI{
			isolationL2VNI("t1-net-1", 30201, "10.231.1.1/24", 10),
			isolationL2VNI("a-dupvni", 30201, "10.231.8.1/24", 20),
		},
	},
	{
		name: "route target with a target: prefix",
		l2vnis: []*v1alpha1.L2VNI{
			isolationL2VNI("t1-net-1", 30201, "10.231.1.1/24", 10),
			withExportRT(isolationL2VNI("a-badrt", 30299, "10.231.8.1/24", 20), "target:65000:30299"),
		},
	},
	{
		name: "nodeSelector that cannot be parsed",
		l2vnis: []*v1alpha1.L2VNI{
			isolationL2VNI("t1-net-1", 30201, "10.231.1.1/24", 10),
			withSelector(isolationL2VNI("a-badsel", 30299, "10.231.8.1/24", 20),
				&metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "pe", Operator: metav1.LabelSelectorOpIn},
				}}),
		},
	},
}

// T16 forced case, and the operator-role review's RT case: an unrelated write
// is admitted while two stored objects conflict on a node.
func TestL2VNIWriteIsAdmittedBesideAStoredConflict(t *testing.T) {
	for _, tc := range storedConflicts {
		t.Run(tc.name, func(t *testing.T) {
			useStoredObjects(t, []*v1.Node{peNode()}, []*v1alpha1.L3VNI{isolationL3VNI(storedL3, "vrfB", 10001)},
				tc.l2vnis)

			unrelated := isolationL2VNI("t1-net-3", 30203, "10.231.3.1/24", -1)
			if err := validateL2VNICreate(unrelated); err != nil {
				t.Errorf("create of an unrelated L2VNI denied: %v", err)
			}
		})
	}
}

func TestL3VNIWriteIsAdmittedBesideAStoredConflict(t *testing.T) {
	for _, tc := range storedConflicts {
		t.Run(tc.name, func(t *testing.T) {
			useStoredObjects(t, []*v1.Node{peNode()}, []*v1alpha1.L3VNI{isolationL3VNI(storedL3, "vrfB", 10001)},
				tc.l2vnis)

			unrelated := isolationL3VNI("vrf-c", "vrfC", 10002)
			unrelated.CreationTimestamp = metav1.Time{}
			if err := validateL3VNICreate(unrelated); err != nil {
				t.Errorf("create of an unrelated L3VNI denied: %v", err)
			}
		})
	}
}

func TestL3VPNWriteIsAdmittedBesideAStoredConflict(t *testing.T) {
	for _, tc := range storedConflicts {
		t.Run(tc.name, func(t *testing.T) {
			useStoredObjects(t, []*v1.Node{peNode()}, []*v1alpha1.L3VNI{isolationL3VNI(storedL3, "vrfB", 10001)},
				tc.l2vnis)

			unrelated := &v1alpha1.L3VPN{
				ObjectMeta: metav1.ObjectMeta{Namespace: isolationNS, Name: "vpn-c"},
				// Dormant: an L3VPN on the node would need an SRv6 underlay.
				Spec: v1alpha1.L3VPNSpec{VRF: "vrfC", RDAssignedNumber: 10003,
					NodeSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"example.com/vpn-c": "true"}},
					ImportRTs:    []v1alpha1.RouteTarget{"100:100"}},
			}
			if err := validateL3VPNCreate(unrelated); err != nil {
				t.Errorf("create of an unrelated L3VPN denied: %v", err)
			}
		})
	}
}

// The other half: a write that is itself the conflict is still denied, and
// the denial names it.
func TestL2VNIWriteThatConflictsIsDenied(t *testing.T) {
	stored := []*v1alpha1.L2VNI{
		isolationL2VNI("t1-net-1", 30201, "10.231.1.1/24", 10),
	}
	tcs := []struct {
		name    string
		written *v1alpha1.L2VNI
		want    string
	}{
		{
			name:    "subnet overlap",
			written: isolationL2VNI("a-overlap", 30299, "10.231.1.129/25", -1),
			want:    `IPNet 10.231.1.128/25 (L2VNI openperouter-system/a-overlap) overlaps with IPNet 10.231.1.0/24 (L2VNI openperouter-system/t1-net-1)`,
		},
		{
			name:    "duplicate VNI",
			written: isolationL2VNI("a-dupvni", 30201, "10.231.8.1/24", -1),
			want:    `L2VNI/a-dupvni: duplicate vni 30201:L2VNI/t1-net-1`,
		},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			useStoredObjects(t, []*v1.Node{peNode()}, []*v1alpha1.L3VNI{isolationL3VNI(storedL3, "vrfB", 10001)},
				stored)
			err := validateL2VNICreate(tc.written)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("create: got %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

// A stored conflict on the node hides no failure of the write: here the node
// already has a duplicate VNI, and the written L2VNI overlaps a subnet.
func TestL2VNIWriteThatConflictsIsDeniedBesideAStoredConflict(t *testing.T) {
	useStoredObjects(t, []*v1.Node{peNode()}, []*v1alpha1.L3VNI{isolationL3VNI(storedL3, "vrfB", 10001)},
		[]*v1alpha1.L2VNI{
			isolationL2VNI("t1-net-1", 30201, "10.231.1.1/24", 10),
			isolationL2VNI("a-dupvni", 30201, "10.231.8.1/24", 20),
		})

	err := validateL2VNICreate(isolationL2VNI("a-overlap", 30299, "10.231.1.129/25", -1))
	want := `IPNet 10.231.1.128/25 (L2VNI openperouter-system/a-overlap) overlaps with IPNet 10.231.1.0/24`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("create: got %v, want an error containing %q", err, want)
	}
	if err != nil && strings.Contains(err.Error(), "a-dupvni") {
		t.Errorf("the denial blames the stored conflict too: %v", err)
	}
}

// An update that would make the object win a conflict it did not have, and
// so take the configuration away from one in service, is denied too.
func TestL2VNIUpdateThatDisplacesAnotherIsDenied(t *testing.T) {
	old := isolationL2VNI("t1-old", 30201, "10.231.1.1/24", 10)
	newer := isolationL2VNI("t1-newer", 30202, "10.231.2.1/24", 20)
	useStoredObjects(t, []*v1.Node{peNode()}, []*v1alpha1.L3VNI{isolationL3VNI(storedL3, "vrfB", 10001)},
		[]*v1alpha1.L2VNI{old, newer})

	updated := old.DeepCopy()
	updated.Spec.VNI = 30202
	err := validateL2VNIUpdate(old, updated)
	if err == nil || !strings.Contains(err.Error(), "L2VNI/t1-newer") {
		t.Errorf("update: got %v, want a denial naming the displaced L2VNI/t1-newer", err)
	}
}

// Case (e): the object's own fields are checked whether or not it selects a
// node today. A demand-mode L2VNI selects none until its first activation.
func TestDormantL2VNIWithABadRouteTargetIsDenied(t *testing.T) {
	useStoredObjects(t, []*v1.Node{peNode()}, []*v1alpha1.L3VNI{isolationL3VNI(storedL3, "vrfB", 10001)}, nil)

	dormant := withSelector(withExportRT(isolationL2VNI("dormant", 30299, "10.231.8.1/24", -1),
		"target:65000:30299"), &metav1.LabelSelector{MatchLabels: map[string]string{"example.com/30299": "true"}})
	err := validateL2VNICreate(dormant)
	if err == nil || !strings.Contains(err.Error(), `invalid route targets for vni "dormant"`) {
		t.Errorf("create: got %v, want an invalid route target error", err)
	}
}

func TestDormantL2VNIWithABadSelectorIsDenied(t *testing.T) {
	useStoredObjects(t, []*v1.Node{peNode()}, []*v1alpha1.L3VNI{isolationL3VNI(storedL3, "vrfB", 10001)}, nil)

	bad := withSelector(isolationL2VNI("badsel", 30299, "10.231.8.1/24", -1),
		&metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "pe", Operator: metav1.LabelSelectorOpIn},
		}})
	err := validateL2VNICreate(bad)
	if err == nil || !strings.Contains(err.Error(), "badsel") {
		t.Errorf("create: got %v, want a denial naming the L2VNI", err)
	}
}
