// SPDX-License-Identifier:Apache-2.0

package bridgerefresh

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/openperouter/openperouter/internal/hostnetwork"
)

const (
	// DefaultRefreshPeriod is how often to check and refresh neighbor entries.
	DefaultRefreshPeriod = 30 * time.Second
)

// StartOptions configures optional parameters for BridgeRefresher.
type StartOptions struct {
	RefreshPeriod time.Duration // Override DefaultRefreshPeriod (for testing)
}

// BridgeRefresher manages neighbor refresh for an L2VNI bridge.
// It makes sure that the STALE neighbors on the bridge are pinged once per
// refresh period, to prevent EVPN Type-2 routes from being withdrawn and to
// force STALE neighbors to FAILED state in case the entry is really STALE.
//
// A BridgeRefresher runs no loop of its own: Start adds its bridge to the
// sweep of its namespace and period, which lists the neighbors of the whole
// namespace once per tick for all of its bridges (see sweep.go).
type BridgeRefresher struct {
	bridgeName    string // e.g., "br-pe-110"
	namespace     string // Path to network namespace
	refreshPeriod time.Duration
	vni           int32

	cancel   context.CancelFunc
	wg       sync.WaitGroup
	stopOnce sync.Once
}

// New creates a new BridgeRefresher for an L2VNI.
// Call Start to begin refreshing.
func New(params hostnetwork.L2VNIParams, opts StartOptions) (*BridgeRefresher, error) {
	refreshPeriod := DefaultRefreshPeriod
	if opts.RefreshPeriod > 0 {
		refreshPeriod = opts.RefreshPeriod
	}

	refresher := &BridgeRefresher{
		bridgeName:    hostnetwork.BridgeName(params.VNI),
		namespace:     params.TargetNS,
		refreshPeriod: refreshPeriod,
		vni:           params.VNI,
	}
	return refresher, nil
}

// Start adds the bridge to the sweep of its namespace until Stop is called
// or ctx is cancelled.
func (r *BridgeRefresher) Start(ctx context.Context) {
	ctx, r.cancel = context.WithCancel(ctx)
	join(r)
	r.wg.Add(1)
	context.AfterFunc(ctx, func() {
		defer r.wg.Done()
		leave(r)
	})

	slog.Info("started bridge refresher", "bridge", r.bridgeName, "vni", r.vni)
}

// Stop removes the bridge from its sweep and waits until that is done.
// It is safe to call multiple times.
func (r *BridgeRefresher) Stop() {
	r.stopOnce.Do(func() {
		r.cancel()
		r.wg.Wait()
		slog.Info("stopped bridge refresher", "bridge", r.bridgeName)
	})
}
