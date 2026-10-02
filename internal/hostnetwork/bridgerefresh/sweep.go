// SPDX-License-Identifier:Apache-2.0

package bridgerefresh

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/openperouter/openperouter/internal/netnamespace"
	"github.com/vishvananda/netns"
)

// A sweep refreshes every bridge of one namespace with one goroutine and one
// neighbor dump per tick. A neighbor dump costs in proportion to the whole
// namespace, under RTNL, so a dump per bridge would make a tick cost
// N(bridges) x N(neighbors) of RTNL and hold off everything else on the node
// that needs it.
//
// The sweep opens the namespace on each tick and closes it after (open, use,
// close) rather than holding an fd, so that a namespace deleted from outside
// does not live on as a zombie.
type sweep struct {
	key     sweepKey
	members map[string]*BridgeRefresher // bridge name -> refresher, guarded by sweepsMu
	cancel  context.CancelFunc
	done    chan struct{}
}

type sweepKey struct {
	namespace string
	period    time.Duration
}

var (
	sweeps   = map[sweepKey]*sweep{}
	sweepsMu sync.Mutex
)

// join adds the refresher's bridge to the sweep of its namespace and period,
// and starts that sweep if it is the first.
func join(r *BridgeRefresher) {
	sweepsMu.Lock()
	defer sweepsMu.Unlock()

	key := sweepKey{namespace: r.namespace, period: r.refreshPeriod}
	s, ok := sweeps[key]
	if !ok {
		ctx, cancel := context.WithCancel(context.Background())
		s = &sweep{key: key, members: map[string]*BridgeRefresher{}, cancel: cancel, done: make(chan struct{})}
		sweeps[key] = s
		go s.run(ctx)
	}
	s.members[r.bridgeName] = r
}

// leave removes the refresher's bridge from its sweep, and stops the sweep
// and waits for it if that was the last bridge.
func leave(r *BridgeRefresher) {
	sweepsMu.Lock()
	key := sweepKey{namespace: r.namespace, period: r.refreshPeriod}
	s, ok := sweeps[key]
	if !ok || s.members[r.bridgeName] != r {
		sweepsMu.Unlock()
		return
	}
	delete(s.members, r.bridgeName)
	if len(s.members) > 0 {
		sweepsMu.Unlock()
		return
	}
	delete(sweeps, key)
	sweepsMu.Unlock()

	// Outside the lock: a tick in progress takes it to read the members.
	s.cancel()
	<-s.done
}

// nextTick returns how long to wait for the next tick: the period less up to
// a fifth of it at random. It never exceeds the period, so a neighbor is still
// pinged at least once per period, and it keeps the sweep out of phase with
// the other periodic work on the node.
func nextTick(period time.Duration) time.Duration {
	return period - rand.N(period/5+1)
}

func (s *sweep) run(ctx context.Context) {
	defer close(s.done)

	timer := time.NewTimer(nextTick(s.key.period))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			s.refresh(ctx)
			timer.Reset(nextTick(s.key.period))
		}
	}
}

// bridges returns the bridges of the sweep at this moment.
func (s *sweep) bridges() map[string]*BridgeRefresher {
	sweepsMu.Lock()
	defer sweepsMu.Unlock()
	bridges := make(map[string]*BridgeRefresher, len(s.members))
	for name, r := range s.members {
		bridges[name] = r
	}
	return bridges
}

// refresh performs a single refresh cycle for every bridge of the sweep.
func (s *sweep) refresh(ctx context.Context) {
	bridges := s.bridges()
	if len(bridges) == 0 {
		return
	}

	ns, err := netns.GetFromPath(s.key.namespace)
	if err != nil {
		slog.Debug("failed to get namespace for refresh", "namespace", s.key.namespace, "error", err)
		return
	}
	defer func() {
		if err := ns.Close(); err != nil {
			slog.Debug("failed to close namespace", "namespace", s.key.namespace, "error", err)
		}
	}()

	if err := netnamespace.In(ns, func() error {
		refreshStaleNeighbors(ctx, bridges)
		return nil
	}); err != nil {
		slog.Debug("failed to execute refresh in namespace", "namespace", s.key.namespace, "error", err)
	}
}

// refreshStaleNeighbors sends an ICMP ping to every STALE neighbor of the
// given bridges in the current namespace. The pings go out one at a time from
// this goroutine: a ping is a socket and a write, with no RTNL, and the socket
// has to be opened in this thread's namespace. It returns how many STALE
// neighbors it found and how many of them it pinged.
func refreshStaleNeighbors(ctx context.Context, bridges map[string]*BridgeRefresher) (found, pinged int) {
	names := make(map[string]bool, len(bridges))
	for name := range bridges {
		names[name] = true
	}

	start := time.Now()
	stale, err := collectStaleNeighbors(names)
	if err != nil {
		slog.Debug("failed to list stale neighbors", "bridges", len(bridges), "error", err)
	}

	for name, neighbors := range stale {
		r := bridges[name]
		found += len(neighbors)
		for _, neigh := range neighbors {
			if ctx.Err() != nil {
				return found, pinged
			}
			slog.Debug("pinging stale neighbor", "ip", neigh.IP, "bridge", name)
			if err := r.sendPing(neigh.IP); err != nil {
				slog.Debug("failed to ping neighbor", "ip", neigh.IP, "bridge", name, "error", err)
				continue
			}
			pinged++
		}
	}
	slog.Debug("refreshed stale neighbors", "bridges", len(bridges), "stale", found, "pinged", pinged, "took", time.Since(start))
	return found, pinged
}
