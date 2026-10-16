// SPDX-License-Identifier:Apache-2.0

package routerconfiguration

import (
	"context"
	"sync"
	"testing"
	"time"

	libovsclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/openperouter/openperouter/internal/hostnetwork"
	"github.com/openperouter/openperouter/internal/ovsdbtest"
	"github.com/openperouter/openperouter/internal/ovsmodel"
)

func TestRepairRunnables(t *testing.T) {
	events := make(chan event.GenericEvent, 1)

	if got := (&PERouterReconciler{}).repairRunnables(events); len(got) != 0 {
		t.Fatalf("expected no runnables by default, got %d", len(got))
	}
	got := (&PERouterReconciler{WatchOVSPorts: true, ResyncPeriod: time.Minute}).repairRunnables(events)
	if len(got) != 2 {
		t.Fatalf("expected two runnables, got %d", len(got))
	}
	watch, ok := got[0].(*ovsPortWatch)
	if !ok {
		t.Fatalf("expected the OVS port watch, got %T", got[0])
	}
	if watch.minGap != minOVSRepairGap {
		t.Fatalf("expected the watch to space its passes by %v, got %v", minOVSRepairGap, watch.minGap)
	}
	resync, ok := got[1].(*periodicResync)
	if !ok {
		t.Fatalf("expected the periodic resync, got %T", got[1])
	}
	if resync.period != time.Minute {
		t.Fatalf("expected a resync every minute, got %v", resync.period)
	}
}

func TestPeriodicResyncQueuesTheNodeEveryPeriod(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan event.GenericEvent, 1)
	resync := &periodicResync{period: 20 * time.Millisecond, events: events}
	resyncDone := make(chan error)
	go func() { resyncDone <- resync.Start(ctx) }()

	for i := range 3 {
		select {
		case <-events:
		case <-time.After(5 * time.Second):
			t.Fatalf("no resync event %d", i+1)
		}
	}

	cancel()
	select {
	case err := <-resyncDone:
		if err != nil {
			t.Fatalf("expected the resync to stop cleanly, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the resync did not stop with its context")
	}
}

// What the OVS port watch reports reaches the workqueue as the node's request,
// through the same source the controller watches.
func TestOVSPortWatchQueuesTheNode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[ctrl.Request]())
	defer q.ShutDown()
	events := make(chan event.GenericEvent, 1)
	if err := source.Channel(events, enqueueForNode("node-a")).Start(ctx, q); err != nil {
		t.Fatalf("failed to start the source: %v", err)
	}

	watch := &ovsPortWatch{events: events, watch: func(ctx context.Context, notify func()) {
		for range 3 {
			notify()
		}
		<-ctx.Done()
	}}
	watchDone := make(chan error)
	go func() { watchDone <- watch.Start(ctx) }()

	item := getWithTimeout(t, q)
	want := ctrl.Request{NamespacedName: types.NamespacedName{Name: "node-a"}}
	if item != want {
		t.Fatalf("expected %v, got %v", want, item)
	}
	q.Done(item)

	cancel()
	select {
	case err := <-watchDone:
		if err != nil {
			t.Fatalf("expected the watch to stop cleanly, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the watch did not stop with its context")
	}
}

// A burst of changes starts one pass at once and at most one more, after the
// gap, for everything that changed meanwhile.
func TestOVSPortWatchSpacesABurstOfChanges(t *testing.T) {
	const minGap = 500 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan event.GenericEvent, 1)
	watch := &ovsPortWatch{events: events, minGap: minGap, watch: func(ctx context.Context, notify func()) {
		for range 1000 {
			notify()
		}
		<-ctx.Done()
	}}
	passes := countEvents(ctx, events)
	go func() { _ = watch.Start(ctx) }()

	require.Eventually(t, func() bool { return len(passes()) >= 1 }, minGap/2, 5*time.Millisecond,
		"expected the burst to start a pass at once")
	require.Never(t, func() bool { return len(passes()) > 1 }, minGap/2, 5*time.Millisecond,
		"expected one pass until the gap is over")
	require.Never(t, func() bool { return len(passes()) > 2 }, 3*minGap, 10*time.Millisecond,
		"expected a burst of 1000 changes to start at most 2 passes")
}

// Another OVSDB writer that keeps breaking a host port the controller manages
// gets one pass per gap, not one per change.
func TestOVSPortWatchSpacesPassesWhileAnotherWriterFlapsAPort(t *testing.T) {
	const (
		minGap   = 200 * time.Millisecond
		flapping = 2 * time.Second
		portName = hostnetwork.HostVethPrefix + hostnetwork.EvpnInfix + "100"
	)
	saved := hostnetwork.OVSSocketPath
	hostnetwork.OVSSocketPath = "unix:" + ovsdbtest.Serve(t)
	t.Cleanup(func() { hostnetwork.OVSSocketPath = saved })
	writer := connectOVSWriter(t)
	createOVSHostPort(t, writer, portName)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan event.GenericEvent, 1)
	watch := &ovsPortWatch{events: events, watch: hostnetwork.WatchL2VNIHostPorts, minGap: minGap}
	passes := countEvents(ctx, events)
	go func() { _ = watch.Start(ctx) }()

	// Each flap makes the interface report an error, then clears it.
	flap := func() {
		setOVSInterfaceError(t, writer, portName, "could not open network device "+portName)
		setOVSInterfaceError(t, writer, portName, "")
	}
	// The first pass shows that the watch is connected.
	poll := time.NewTicker(20 * time.Millisecond)
	defer poll.Stop()
	for deadline := time.Now().Add(10 * time.Second); len(passes()) == 0; <-poll.C {
		if time.Now().After(deadline) {
			t.Fatal("the watch never started a pass for a broken host port")
		}
		flap()
	}

	start, changes := len(passes()), 0
	fast := time.NewTicker(5 * time.Millisecond)
	defer fast.Stop()
	for end := time.Now().Add(flapping); time.Now().Before(end); changes++ {
		flap()
		<-fast.C
	}
	// One pass per gap, and one at most for the changes of the last gap, which
	// the watch may still hold back.
	bound := int(flapping/minGap) + 1
	require.Never(t, func() bool { return len(passes())-start > bound }, 2*minGap, 10*time.Millisecond,
		"expected at most %d passes while the port flapped %d times in %v", bound, changes, flapping)

	if got := len(passes()) - start; got < bound/2 {
		t.Fatalf("expected the repairs to go on while the port flapped, got %d passes in %v", got, flapping)
	}
}

func TestNotifyNodeDoesNotBlock(t *testing.T) {
	events := make(chan event.GenericEvent, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 10 {
			notifyNode(events, "test")
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("notifyNode blocked on a full channel")
	}
	if got := len(events); got != 1 {
		t.Fatalf("expected one pending event, got %d", got)
	}
}

func getWithTimeout(t *testing.T, q workqueue.TypedRateLimitingInterface[ctrl.Request]) ctrl.Request {
	t.Helper()
	got := make(chan ctrl.Request, 1)
	go func() {
		item, _ := q.Get()
		got <- item
	}()
	select {
	case item := <-got:
		return item
	case <-time.After(5 * time.Second):
		t.Fatal("no request was queued")
		return ctrl.Request{}
	}
}

// countEvents consumes events as the controller would, and returns what
// reports when each arrived.
func countEvents(ctx context.Context, events <-chan event.GenericEvent) func() []time.Time {
	var mu sync.Mutex
	var at []time.Time
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-events:
				mu.Lock()
				at = append(at, time.Now())
				mu.Unlock()
			}
		}
	}()
	return func() []time.Time {
		mu.Lock()
		defer mu.Unlock()
		return append([]time.Time(nil), at...)
	}
}

func connectOVSWriter(t *testing.T) libovsclient.Client {
	t.Helper()
	ovs, err := hostnetwork.NewOVSClient(context.Background())
	if err != nil {
		t.Fatalf("failed to connect to OVSDB: %v", err)
	}
	t.Cleanup(ovs.Close)
	return ovs
}

// createOVSHostPort creates a bridge holding one port, without the kernel side
// that ovs-vswitchd would add.
func createOVSHostPort(t *testing.T, ovs libovsclient.Client, portName string) {
	t.Helper()
	rows := []model.Model{
		&ovsmodel.Interface{UUID: "new_interface", Name: portName, Type: "system"},
		&ovsmodel.Port{UUID: "new_port", Name: portName, Interfaces: []string{"new_interface"}},
		&ovsmodel.Bridge{UUID: "new_bridge", Name: "br-test", Ports: []string{"new_port"}},
		&ovsmodel.OpenvSwitch{UUID: "new_root", Bridges: []string{"new_bridge"}},
	}
	ops := make([]ovsdb.Operation, 0, len(rows))
	for _, row := range rows {
		create, err := ovs.Create(row)
		if err != nil {
			t.Fatalf("failed to build the insert of %T: %v", row, err)
		}
		ops = append(ops, create...)
	}
	transactOVS(t, ovs, ops...)
}

func setOVSInterfaceError(t *testing.T, ovs libovsclient.Client, name, message string) {
	t.Helper()
	iface := &ovsmodel.Interface{Name: name, Error: &message}
	ops, err := ovs.Where(iface).Update(iface, &iface.Error)
	if err != nil {
		t.Fatalf("failed to build the error update: %v", err)
	}
	transactOVS(t, ovs, ops...)
}

func transactOVS(t *testing.T, ovs libovsclient.Client, ops ...ovsdb.Operation) {
	t.Helper()
	reply, err := ovs.Transact(context.Background(), ops...)
	if err != nil {
		t.Fatalf("transaction failed: %v", err)
	}
	if _, err := ovsdb.CheckOperationResults(reply, ops); err != nil {
		t.Fatalf("transaction failed: %v", err)
	}
}
