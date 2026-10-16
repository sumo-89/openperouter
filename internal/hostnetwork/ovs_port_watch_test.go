// SPDX-License-Identifier:Apache-2.0

package hostnetwork

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	libovsclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
	"github.com/stretchr/testify/require"

	"github.com/openperouter/openperouter/internal/ovsdbtest"
	"github.com/openperouter/openperouter/internal/ovsmodel"
)

func TestL2VNIHostPortChangeFilter(t *testing.T) {
	tests := []struct {
		name   string
		table  string
		old    model.Model
		newRow model.Model
		want   bool
	}{
		{
			name:  "host port deleted",
			table: ovsmodel.PortTable,
			old:   &ovsmodel.Port{Name: "host-e-100"},
			want:  true,
		},
		{
			name:  "host interface deleted",
			table: ovsmodel.InterfaceTable,
			old:   &ovsmodel.Interface{Name: "host-e-100"},
			want:  true,
		},
		{
			name:  "unrelated port deleted",
			table: ovsmodel.PortTable,
			old:   &ovsmodel.Port{Name: "patch-br-int"},
		},
		{
			name:  "SRv6 host port deleted",
			table: ovsmodel.PortTable,
			old:   &ovsmodel.Port{Name: "host-s-100"},
		},
		{
			name:   "host port inserted",
			table:  ovsmodel.PortTable,
			newRow: &ovsmodel.Port{Name: "host-e-100"},
		},
		{
			name:   "host port updated",
			table:  ovsmodel.PortTable,
			old:    &ovsmodel.Port{Name: "host-e-100"},
			newRow: &ovsmodel.Port{Name: "host-e-100", Tag: new(12)},
		},
		{
			name:   "host interface reports an error",
			table:  ovsmodel.InterfaceTable,
			old:    &ovsmodel.Interface{Name: "host-e-100"},
			newRow: &ovsmodel.Interface{Name: "host-e-100", Error: new("could not open network device host-e-100")},
			want:   true,
		},
		{
			name:   "host interface error cleared",
			table:  ovsmodel.InterfaceTable,
			old:    &ovsmodel.Interface{Name: "host-e-100", Error: new("could not open network device host-e-100")},
			newRow: &ovsmodel.Interface{Name: "host-e-100"},
		},
		{
			name:   "unrelated interface reports an error",
			table:  ovsmodel.InterfaceTable,
			old:    &ovsmodel.Interface{Name: "eth1"},
			newRow: &ovsmodel.Interface{Name: "eth1", Error: new("gone")},
		},
		{
			name:  "other table",
			table: ovsmodel.BridgeTable,
			old:   &ovsmodel.Bridge{Name: "host-e-100"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isL2VNIHostPortChange(tt.table, tt.old, tt.newRow); got != tt.want {
				t.Errorf("isL2VNIHostPortChange() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Without OVSDB the watch keeps retrying, never notifies, and returns as soon
// as its context is done: a node without OVS must not block the controller.
func TestWatchL2VNIHostPortsWithoutOVSDB(t *testing.T) {
	useOVSSocket(t, t.TempDir()+"/no-such-db.sock")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var notified atomic.Int32
	retry := portWatchRetry{initial: 10 * time.Millisecond, max: 20 * time.Millisecond, liveness: time.Second}

	done := make(chan struct{})
	go func() {
		defer close(done)
		watchL2VNIHostPorts(ctx, func() { notified.Add(1) }, retry)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watch did not return after its context was done")
	}
	if got := notified.Load(); got != 0 {
		t.Fatalf("expected no notification without OVSDB, got %d", got)
	}
}

func TestWatchL2VNIHostPorts(t *testing.T) {
	const hostPort = HostVethPrefix + EvpnInfix + "100"
	path, restart := ovsdbtest.ServeRestartable(t)
	useOVSSocket(t, path)
	writer := connectOVSWriter(t)
	bridge := createOVSBridge(t, writer, "br-test")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var notified atomic.Int32
	retry := portWatchRetry{initial: 50 * time.Millisecond, max: 200 * time.Millisecond, liveness: time.Second}
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchL2VNIHostPorts(ctx, func() { notified.Add(1) }, retry)
	}()
	waitUntilWatched := func(writer libovsclient.Client) {
		t.Helper()
		require.Eventually(t, func() bool {
			if notified.Load() > 0 {
				return true
			}
			addOVSPort(t, writer, bridge, hostPort)
			deleteOVSPort(t, writer, bridge, hostPort)
			return false
		}, 10*time.Second, 100*time.Millisecond, "the watch never reported a deleted host port")
	}
	waitUntilWatched(writer)

	t.Run("ignores ports that are not an L2VNI's", func(t *testing.T) {
		notified.Store(0)
		addOVSPort(t, writer, bridge, "other-port")
		deleteOVSPort(t, writer, bridge, "other-port")
		require.Never(t, func() bool { return notified.Load() > 0 }, 500*time.Millisecond, 20*time.Millisecond)
	})

	t.Run("notifies when an L2VNI host port is deleted", func(t *testing.T) {
		addOVSPort(t, writer, bridge, hostPort)
		require.Never(t, func() bool { return notified.Load() > 0 }, 200*time.Millisecond, 20*time.Millisecond,
			"an inserted port must not notify")
		deleteOVSPort(t, writer, bridge, hostPort)
		require.Eventually(t, func() bool { return notified.Load() > 0 }, 5*time.Second, 20*time.Millisecond)
	})

	t.Run("notifies when the interface of an L2VNI host port reports an error", func(t *testing.T) {
		addOVSPort(t, writer, bridge, hostPort)
		notified.Store(0)
		setOVSInterfaceError(t, writer, hostPort, "could not open network device "+hostPort)
		require.Eventually(t, func() bool { return notified.Load() > 0 }, 5*time.Second, 20*time.Millisecond)
		deleteOVSPort(t, writer, bridge, hostPort)
	})

	t.Run("reconnects after ovsdb-server restarts and notifies once it is back", func(t *testing.T) {
		notified.Store(0)
		restart()
		require.Eventually(t, func() bool { return notified.Load() > 0 }, 10*time.Second, 20*time.Millisecond,
			"expected a notification once the watch is back")

		writer := connectOVSWriter(t)
		bridge = createOVSBridge(t, writer, "br-test")
		notified.Store(0)
		waitUntilWatched(writer)
	})

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watch did not return after its context was done")
	}
}

func useOVSSocket(t *testing.T, path string) {
	saved := OVSSocketPath
	OVSSocketPath = "unix:" + path
	t.Cleanup(func() { OVSSocketPath = saved })
}

func connectOVSWriter(t *testing.T) libovsclient.Client {
	t.Helper()
	ovs, err := NewOVSClient(context.Background())
	require.NoError(t, err)
	t.Cleanup(ovs.Close)
	_, err = ovs.MonitorAll(context.Background())
	require.NoError(t, err)
	return ovs
}

// createOVSBridge creates the database root and an empty bridge, and returns
// the bridge's UUID.
func createOVSBridge(t *testing.T, ovs libovsclient.Client, name string) string {
	t.Helper()
	bridge := &ovsmodel.Bridge{UUID: "new_bridge", Name: name}
	ops := ovsCreate(t, ovs, bridge, &ovsmodel.OpenvSwitch{UUID: "new_root", Bridges: []string{"new_bridge"}})
	reply := ovsTransact(t, ovs, ops...)
	return reply[0].UUID.GoUUID
}

// addOVSPort adds a port with one interface to the bridge, without the kernel
// side that ovs-vswitchd would add.
func addOVSPort(t *testing.T, ovs libovsclient.Client, bridgeUUID, name string) {
	t.Helper()
	ops := ovsCreate(t, ovs,
		&ovsmodel.Interface{UUID: "new_interface", Name: name, Type: "system"},
		&ovsmodel.Port{UUID: "new_port", Name: name, Interfaces: []string{"new_interface"}})
	bridge := &ovsmodel.Bridge{UUID: bridgeUUID}
	mutate, err := ovs.Where(bridge).Mutate(bridge, model.Mutation{
		Field: &bridge.Ports, Mutator: ovsdb.MutateOperationInsert, Value: []string{"new_port"},
	})
	require.NoError(t, err)
	ovsTransact(t, ovs, append(ops, mutate...)...)
}

func deleteOVSPort(t *testing.T, ovs libovsclient.Client, bridgeUUID, name string) {
	t.Helper()
	port := &ovsmodel.Port{Name: name}
	require.NoError(t, ovs.Get(context.Background(), port))
	bridge := &ovsmodel.Bridge{UUID: bridgeUUID}
	ops, err := ovs.Where(bridge).Mutate(bridge, model.Mutation{
		Field: &bridge.Ports, Mutator: ovsdb.MutateOperationDelete, Value: []string{port.UUID},
	})
	require.NoError(t, err)
	deletePort, err := ovs.Where(port).Delete()
	require.NoError(t, err)
	deleteIface, err := ovs.Where(&ovsmodel.Interface{Name: name}).Delete()
	require.NoError(t, err)
	ops = append(ops, deletePort...)
	ovsTransact(t, ovs, append(ops, deleteIface...)...)
}

func setOVSInterfaceError(t *testing.T, ovs libovsclient.Client, name, message string) {
	t.Helper()
	iface := &ovsmodel.Interface{Name: name, Error: &message}
	ops, err := ovs.Where(iface).Update(iface, &iface.Error)
	require.NoError(t, err)
	ovsTransact(t, ovs, ops...)
}

func ovsCreate(t *testing.T, ovs libovsclient.Client, rows ...model.Model) []ovsdb.Operation {
	t.Helper()
	ops := make([]ovsdb.Operation, 0, len(rows))
	for _, row := range rows {
		create, err := ovs.Create(row)
		require.NoError(t, err)
		ops = append(ops, create...)
	}
	return ops
}

func ovsTransact(t *testing.T, ovs libovsclient.Client, ops ...ovsdb.Operation) []ovsdb.OperationResult {
	t.Helper()
	reply, err := ovs.Transact(context.Background(), ops...)
	require.NoError(t, err)
	_, err = ovsdb.CheckOperationResults(reply, ops)
	require.NoError(t, err)
	return reply
}
