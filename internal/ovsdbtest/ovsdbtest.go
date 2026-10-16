// SPDX-License-Identifier:Apache-2.0

// Package ovsdbtest serves OVSDB to tests: an in-memory Open_vSwitch database
// for tests that need no ovs-vswitchd, and a proxy that counts the connections
// made to a database.
package ovsdbtest

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"github.com/ovn-kubernetes/libovsdb/database/inmemory"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/server"

	"github.com/openperouter/openperouter/internal/ovsmodel"
)

// T is the part of testing.TB the helpers use, so that ginkgo's GinkgoT()
// works too.
type T interface {
	Helper()
	Fatalf(format string, args ...any)
	Cleanup(func())
}

// Serve starts an in-memory Open_vSwitch database and returns the path of its
// unix socket. The database is stopped when the test ends.
func Serve(t T) string {
	t.Helper()
	clientModel, err := ovsmodel.FullDatabaseModel()
	if err != nil {
		t.Fatalf("failed to build the OVS client model: %v", err)
	}
	dbModel, errs := model.NewDatabaseModel(ovsmodel.Schema(), clientModel)
	if len(errs) > 0 {
		t.Fatalf("failed to build the OVS database model: %v", errs)
	}
	logger := logr.Discard()
	db := inmemory.NewDatabase(map[string]model.ClientDBModel{clientModel.Name(): clientModel}, &logger)
	srv, err := server.NewOvsdbServer(db, &logger, dbModel)
	if err != nil {
		t.Fatalf("failed to create the OVSDB server: %v", err)
	}

	path := filepath.Join(socketDir(t), "db.sock")
	go func() { _ = srv.Serve("unix", path) }()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.After(5 * time.Second)
	for !srv.Ready() {
		select {
		case <-ticker.C:
		case <-timeout:
			t.Fatalf("the OVSDB server did not start on %s", path)
		}
	}
	t.Cleanup(srv.Close)
	return path
}

// CountConnections listens on a new unix socket that forwards every connection
// to the unix socket at target, and counts the connections. It returns the
// path of the new socket and the counter.
func CountConnections(t T, target string) (string, *atomic.Int32) {
	t.Helper()
	path := filepath.Join(socketDir(t), "proxy.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("failed to listen on %s: %v", path, err)
	}
	var connections atomic.Int32
	var wg sync.WaitGroup
	done := make(chan struct{})
	t.Cleanup(func() {
		close(done)
		_ = listener.Close()
		wg.Wait()
	})
	wg.Go(func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			wg.Go(func() { forward(conn, target, done) })
		}
	})
	return path, &connections
}

func forward(conn net.Conn, target string, done <-chan struct{}) {
	defer func() { _ = conn.Close() }()
	upstream, err := net.Dial("unix", target)
	if err != nil {
		return
	}
	defer func() { _ = upstream.Close() }()

	copied := make(chan error, 2)
	go func() { _, err := io.Copy(upstream, conn); copied <- err }()
	go func() { _, err := io.Copy(conn, upstream); copied <- err }()
	select {
	case <-copied:
	case <-done:
	}
}

func socketDir(t T) string {
	t.Helper()
	// Not t.TempDir(): unix socket paths are limited to about 100 bytes.
	dir, err := os.MkdirTemp("", "ovsdb")
	if err != nil {
		t.Fatalf("failed to create a socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
