// SPDX-License-Identifier:Apache-2.0

package routerconfiguration

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/openperouter/openperouter/internal/hostnetwork"
)

// repairRunnables returns what starts a pass of the node when no Kubernetes
// object changed: a pass rebuilds what it finds broken on the host, but only
// runs on an event. They feed events, and the node's single request coalesces
// them with every other event.
func (r *PERouterReconciler) repairRunnables(events chan<- event.GenericEvent) []manager.Runnable {
	var runnables []manager.Runnable
	if r.WatchOVSPorts {
		runnables = append(runnables, &ovsPortWatch{
			events: events, watch: hostnetwork.WatchL2VNIHostPorts, minGap: minOVSRepairGap,
		})
	}
	if r.ResyncPeriod > 0 {
		runnables = append(runnables, &periodicResync{period: r.ResyncPeriod, events: events})
	}
	return runnables
}

// minOVSRepairGap is the least time between two passes the OVSDB port watch
// starts. Its events bypass the node's rate limiter, so without a gap another
// OVSDB writer that keeps undoing what a pass restores would run full passes
// back to back at its own speed.
const minOVSRepairGap = 2 * time.Second

// ovsPortWatch turns what the OVSDB watch sees into events for the node, for
// as long as the manager runs, at most one every minGap.
type ovsPortWatch struct {
	events chan<- event.GenericEvent
	watch  func(ctx context.Context, notify func())
	minGap time.Duration
}

func (w *ovsPortWatch) Start(ctx context.Context) error {
	changed := make(chan struct{}, 1)
	var changes atomic.Int64
	forwarded := make(chan struct{})
	go func() {
		defer close(forwarded)
		w.forward(ctx, changed, &changes)
	}()
	w.watch(ctx, func() {
		changes.Add(1)
		select {
		case changed <- struct{}{}:
		default:
		}
	})
	<-forwarded
	return nil
}

// forward queues the node for the first change at once, and for every change
// seen while it waits out minGap as one more pass after the gap. Each repair is
// logged with the changes it covers, so that a writer fighting the controller
// shows as a steady stream of them.
func (w *ovsPortWatch) forward(ctx context.Context, changed <-chan struct{}, changes *atomic.Int64) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-changed:
		}
		slog.InfoContext(ctx, "OVSDB shows an L2VNI host port deleted or broken, queueing a repair pass",
			"changes", changes.Swap(0))
		notifyNode(w.events, "ovs-port-change")
		if !sleepCtx(ctx, w.minGap) {
			return
		}
	}
}

// NeedLeaderElection is false: every node's controller repairs its own host.
func (w *ovsPortWatch) NeedLeaderElection() bool {
	return false
}

// periodicResync starts a pass every period, as a backstop for host state
// that changes behind the controller's back and that nothing watches.
type periodicResync struct {
	period time.Duration
	events chan<- event.GenericEvent
}

func (p *periodicResync) Start(ctx context.Context) error {
	ticker := time.NewTicker(p.period)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			notifyNode(p.events, "periodic-resync")
		}
	}
}

// NeedLeaderElection is false: every node's controller repairs its own host.
func (p *periodicResync) NeedLeaderElection() bool {
	return false
}

// notifyNode never blocks. When the channel is full an event is already
// waiting, and the pass it starts begins after this call, so dropping this one
// loses nothing.
func notifyNode(events chan<- event.GenericEvent, reason string) {
	select {
	case events <- event.GenericEvent{Object: &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: reason}}}:
	default:
	}
}

// sleepCtx waits for d and reports whether ctx is still live.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
