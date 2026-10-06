// SPDX-License-Identifier:Apache-2.0

package routerconfiguration

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openperouter/openperouter/api/v1alpha1"
	"github.com/openperouter/openperouter/internal/frrconfig"
)

func TestRouterIdentity(t *testing.T) {
	tests := []struct {
		name string
		pods []client.Object
		want string
	}{
		{"one router pod", []client.Object{routerPod("u1", testNodeName, 2)}, "u1/2"},
		{"no router pod", nil, ""},
		{"router pod on another node", []client.Object{routerPod("u1", "worker-2", 0)}, ""},
		{"two router pods during a rollout", []client.Object{
			routerPod("u1", testNodeName, 0), routerPod("u2", testNodeName, 0)}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestPERouterReconciler(t, &noopDatapathConfigurator{}, testNode())
			r.Client = clientWithPods(t, tc.pods...)
			if got := r.routerIdentity(context.Background(), testLogger()); got != tc.want {
				t.Fatalf("identity = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReconcileReloadsFRROncePerRouter(t *testing.T) {
	pod := routerPod("u1", testNodeName, 0)
	r := newTestPERouterReconciler(t, &noopDatapathConfigurator{}, testNode())
	r.Client = clientWithPods(t, pod)
	socket, reloads := countingFRRSocket(t)
	r.FRRReloadSocket = socket
	r.frrUpdater = frrconfig.NewUpdater(socket, r.FRRConfigPath)

	reconcileTwice(t, r)
	if got := reloads.Load(); got != 1 {
		t.Fatalf("an unchanged configuration must be reloaded once, got %d reloads", got)
	}

	pod.Status.ContainerStatuses[0].RestartCount = 1
	if err := r.Status().Update(context.Background(), pod); err != nil {
		t.Fatalf("failed to update the router pod: %v", err)
	}
	reconcileTwice(t, r)
	if got := reloads.Load(); got != 2 {
		t.Fatalf("a restarted frr container must get the configuration again, got %d reloads", got)
	}
}

func TestPodRouterIdentity(t *testing.T) {
	pod := func(uid types.UID, statuses ...v1.ContainerStatus) *v1.Pod {
		return &v1.Pod{ObjectMeta: metav1.ObjectMeta{UID: uid}, Status: v1.PodStatus{ContainerStatuses: statuses}}
	}
	tests := []struct {
		name string
		pod  *v1.Pod
		want string
	}{
		{"frr container", pod("u1", v1.ContainerStatus{Name: "reloader", RestartCount: 4},
			v1.ContainerStatus{Name: "frr", RestartCount: 2}), "u1/2"},
		{"frr restarted", pod("u1", v1.ContainerStatus{Name: "frr", RestartCount: 3}), "u1/3"},
		{"new pod", pod("u2", v1.ContainerStatus{Name: "frr"}), "u2/0"},
		{"no frr status yet", pod("u3", v1.ContainerStatus{Name: "reloader"}), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := podRouterIdentity(tc.pod); got != tc.want {
				t.Fatalf("identity = %q, want %q", got, tc.want)
			}
		})
	}
}

func routerPod(uid types.UID, node string, frrRestarts int32) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "router-" + string(uid),
			Namespace: testNamespace,
			UID:       uid,
			Labels:    map[string]string{"app": "router"},
		},
		Spec:   v1.PodSpec{NodeName: node},
		Status: v1.PodStatus{ContainerStatuses: []v1.ContainerStatus{{Name: "frr", RestartCount: frrRestarts}}},
	}
}

// clientWithPods registers the pod node-name index the manager registers in
// SetupWithManager, which routerIdentity lists by.
func clientWithPods(t *testing.T, pods ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(append(pods, testNode())...).
		WithStatusSubresource(&v1alpha1.RouterNodeConfigurationStatus{}, &v1.Pod{}).
		WithIndex(&v1.Pod{}, nodeNameIndex, func(o client.Object) []string {
			return []string{o.(*v1.Pod).Spec.NodeName}
		}).
		Build()
}

func reconcileTwice(t *testing.T, r *PERouterReconciler) {
	t.Helper()
	for range 2 {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
			t.Fatalf("Reconcile() returned error: %v", err)
		}
	}
}

func countingFRRSocket(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "frr.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("failed to create unix socket: %v", err)
	}
	var reloads atomic.Int32
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reloads.Add(1)
		w.WriteHeader(http.StatusOK)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return socketPath, &reloads
}
