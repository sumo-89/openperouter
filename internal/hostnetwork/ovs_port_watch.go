// SPDX-License-Identifier:Apache-2.0

package hostnetwork

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	libovscache "github.com/ovn-kubernetes/libovsdb/cache"
	libovsclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"

	"github.com/openperouter/openperouter/internal/ovsmodel"
)

// portWatchRetry bounds how often the watch tries to reach OVSDB. The first
// retry after a lost connection is quick, so that an ovsdb-server restart
// costs seconds; the cap keeps a node without OVS at one attempt per interval.
type portWatchRetry struct {
	initial, max time.Duration
	// liveness is how often a connected watch checks that the server still answers.
	liveness time.Duration
}

var defaultPortWatchRetry = portWatchRetry{initial: time.Second, max: 30 * time.Second, liveness: 30 * time.Second}

// WatchL2VNIHostPorts watches OVSDB for changes to the host ports of L2VNIs
// that only a reconcile can repair, and calls notify for each, until ctx is
// done. Reconciles are started by Kubernetes events, so without the watch a
// host port that something else removed from its bridge stays missing until
// an unrelated event reaches the node.
//
// OVSDB may be absent, on a node whose L2VNIs use Linux bridges, or restart.
// The watch then retries with backoff. After a lost connection it notifies
// once it is back, because what changed in between is unknown.
//
// notify is called from libovsdb's event loop and must not block.
func WatchL2VNIHostPorts(ctx context.Context, notify func()) {
	watchL2VNIHostPorts(ctx, notify, defaultPortWatchRetry)
}

func watchL2VNIHostPorts(ctx context.Context, notify func(), retry portWatchRetry) {
	delay := retry.initial
	failing, notifyWhenBack := false, false
	for {
		ovs, err := connectPortWatch(ctx, notify)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return
			}
			logPortWatchFailure(ctx, failing, err, delay)
			failing = true
		default:
			failing = false
			// A server that drops every connection soon after accepting it
			// must not turn the watch into a stream of notifications, so
			// only a connection that lasted resets the backoff.
			if watchUntilDisconnected(ctx, ovs, notify, notifyWhenBack, retry.liveness) > retry.max {
				delay = retry.initial
			}
		}
		notifyWhenBack = true
		if !sleepCtx(ctx, delay) {
			return
		}
		delay = min(delay*2, retry.max)
	}
}

// watchUntilDisconnected returns how long the connection lasted.
func watchUntilDisconnected(ctx context.Context, ovs libovsclient.Client, notify func(), notifyNow bool,
	liveness time.Duration) time.Duration {
	defer ovs.Close()
	slog.InfoContext(ctx, "watching OVSDB for changes to L2VNI host ports")
	if notifyNow {
		notify()
	}
	start := time.Now()
	waitForDisconnect(ctx, ovs, liveness)
	if ctx.Err() == nil {
		slog.InfoContext(ctx, "lost the OVSDB connection of the L2VNI host port watch, reconnecting")
	}
	return time.Since(start)
}

// connectPortWatch opens a client that monitors only the columns the filter
// reads. The Interface table's statistics change every few seconds on every
// interface; monitoring them would wake the watch for nothing. Bridge.ports is
// not monitored either, so a host port moved to another bridge, whose Port row
// survives, goes unseen here.
func connectPortWatch(ctx context.Context, notify func()) (libovsclient.Client, error) {
	connectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ovs, err := NewOVSClient(connectCtx)
	if err != nil {
		return nil, err
	}
	ovs.Cache().AddEventHandler(&libovscache.EventHandlerFuncs{
		UpdateFunc: func(table string, old, newRow model.Model) {
			if isL2VNIHostPortChange(table, old, newRow) {
				notify()
			}
		},
		DeleteFunc: func(table string, row model.Model) {
			if isL2VNIHostPortChange(table, row, nil) {
				notify()
			}
		},
	})
	port := &ovsmodel.Port{}
	iface := &ovsmodel.Interface{}
	if _, err := ovs.Monitor(connectCtx, ovs.NewMonitor(
		libovsclient.WithTable(port, &port.Name),
		libovsclient.WithTable(iface, &iface.Name, &iface.Error),
	)); err != nil {
		ovs.Close()
		return nil, fmt.Errorf("failed to monitor the OVSDB port tables: %w", err)
	}
	return ovs, nil
}

// waitForDisconnect returns when ctx is done or the connection is gone. libovsdb
// signals a lost connection only to a reader already waiting, so the server is
// also asked for an echo now and then, which covers a signal sent before this
// started waiting and a server that stopped answering.
func waitForDisconnect(ctx context.Context, ovs libovsclient.Client, liveness time.Duration) {
	ticker := time.NewTicker(liveness)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ovs.DisconnectNotify():
			return
		case <-ticker.C:
			if !echoes(ctx, ovs, liveness) {
				return
			}
		}
	}
}

func echoes(ctx context.Context, ovs libovsclient.Client, timeout time.Duration) bool {
	echoCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return ovs.Echo(echoCtx) == nil
}

// logPortWatchFailure logs the first failure of a streak at info level and the
// rest at debug, so that a node without OVS does not log every retry.
func logPortWatchFailure(ctx context.Context, failing bool, err error, retryIn time.Duration) {
	if failing {
		slog.DebugContext(ctx, "OVSDB still unreachable for the L2VNI host port watch", "error", err, "retryIn", retryIn)
		return
	}
	slog.InfoContext(ctx, "OVSDB unreachable for the L2VNI host port watch, retrying with backoff",
		"error", err, "retryIn", retryIn)
}

// isL2VNIHostPortChange tells whether an OVSDB event on the Port or Interface
// table concerns the host port of an L2VNI in a way a reconcile repairs: the
// port or its interface deleted, or its interface reporting an error, which is
// how OVS reports that the veth is gone. old is nil for an insert and newRow
// nil for a delete. Inserts never match: a port that appears is either the
// controller's own or none of its business.
func isL2VNIHostPortChange(table string, old, newRow model.Model) bool {
	if old == nil {
		return false
	}
	switch table {
	case ovsmodel.PortTable:
		return isHostPortDelete(old, newRow)
	case ovsmodel.InterfaceTable:
		return isHostInterfaceChange(old, newRow)
	}
	return false
}

func isHostPortDelete(old, newRow model.Model) bool {
	oldPort, ok := old.(*ovsmodel.Port)
	if !ok {
		return false
	}
	return newRow == nil && isL2VNIHostPortName(oldPort.Name)
}

func isHostInterfaceChange(old, newRow model.Model) bool {
	oldIface, ok := old.(*ovsmodel.Interface)
	if !ok || !isL2VNIHostPortName(oldIface.Name) {
		return false
	}
	if newRow == nil {
		return true
	}
	newIface, ok := newRow.(*ovsmodel.Interface)
	if !ok {
		return false
	}
	return !hasError(oldIface) && hasError(newIface)
}

func isL2VNIHostPortName(name string) bool {
	return strings.HasPrefix(name, HostVethPrefix+EvpnInfix)
}

func hasError(iface *ovsmodel.Interface) bool {
	return iface.Error != nil && *iface.Error != ""
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
