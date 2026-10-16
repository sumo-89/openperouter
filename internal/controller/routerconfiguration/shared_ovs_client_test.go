// SPDX-License-Identifier:Apache-2.0

package routerconfiguration

import (
	"context"
	"testing"

	"github.com/openperouter/openperouter/internal/hostnetwork"
	"github.com/openperouter/openperouter/internal/ovsdbtest"
)

func TestWithSharedOVSClientOpensOneClientForThePass(t *testing.T) {
	proxy, connections := ovsdbtest.CountConnections(t, ovsdbtest.Serve(t))
	setOVSSocketPath(t, "unix:"+proxy)

	ovsMaster := &hostnetwork.HostMaster{Type: hostnetwork.OVSBridgeLinkType}
	l2vnis := []hostnetwork.L2VNIParams{{HostMaster: ovsMaster}, {HostMaster: ovsMaster}, {HostMaster: ovsMaster}}
	ctx := context.Background()

	got, closeOVS := withSharedOVSClient(ctx, l2vnis)
	defer closeOVS()

	if got == ctx {
		t.Fatal("expected a context carrying the shared OVSDB client")
	}
	if n := connections.Load(); n != 1 {
		t.Fatalf("expected one OVSDB connection for the pass, got %d", n)
	}
}

func TestWithSharedOVSClientFallsBackWhenOVSDBIsUnreachable(t *testing.T) {
	setOVSSocketPath(t, "unix:"+t.TempDir()+"/no-such-db.sock")

	l2vnis := []hostnetwork.L2VNIParams{{HostMaster: &hostnetwork.HostMaster{Type: hostnetwork.OVSBridgeLinkType}}}
	ctx := context.Background()

	got, closeOVS := withSharedOVSClient(ctx, l2vnis)
	closeOVS()

	if got != ctx {
		t.Fatal("expected the context to be returned unchanged, so every L2VNI opens its own client")
	}
}

func TestWithSharedOVSClientSkipsNodesWithoutOVSHostMasters(t *testing.T) {
	proxy, connections := ovsdbtest.CountConnections(t, ovsdbtest.Serve(t))
	setOVSSocketPath(t, "unix:"+proxy)

	l2vnis := []hostnetwork.L2VNIParams{
		{HostMaster: &hostnetwork.HostMaster{Type: hostnetwork.BridgeLinkType}},
		{},
	}
	ctx := context.Background()

	got, closeOVS := withSharedOVSClient(ctx, l2vnis)
	closeOVS()

	if got != ctx {
		t.Fatal("expected no client for L2VNIs without an OVS host master")
	}
	if n := connections.Load(); n != 0 {
		t.Fatalf("expected no OVSDB connection, got %d", n)
	}
}

func setOVSSocketPath(t *testing.T, path string) {
	t.Helper()
	saved := hostnetwork.OVSSocketPath
	hostnetwork.OVSSocketPath = path
	t.Cleanup(func() { hostnetwork.OVSSocketPath = saved })
}
