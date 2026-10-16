// SPDX-License-Identifier:Apache-2.0

// Package ovsdbtest serves an in-memory Open_vSwitch database to tests that
// need OVSDB but no ovs-vswitchd and no root.
package ovsdbtest

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
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
	path := filepath.Join(socketDir(t), "db.sock")
	t.Cleanup(serveAt(t, path))
	return path
}

// ServeRestartable is Serve with a restart: restart drops every connection and
// replaces the database with an empty one, as an ovsdb-server restart does.
// The in-memory server keeps its connections open when it is stopped, so the
// clients reach it through a proxy that can drop them.
func ServeRestartable(t T) (string, func()) {
	t.Helper()
	dir := socketDir(t)
	backends := 0
	newBackend := func() string {
		backends++
		path := filepath.Join(dir, fmt.Sprintf("backend-%d.sock", backends))
		t.Cleanup(serveAt(t, path))
		return path
	}
	p := &proxy{target: newBackend(), conns: map[net.Conn]struct{}{}}
	path := filepath.Join(dir, "db.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("failed to listen on %s: %v", path, err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
		p.dropAll()
	})
	go p.serve(listener)
	restart := func() {
		target := newBackend()
		p.mu.Lock()
		p.target = target
		p.mu.Unlock()
		p.dropAll()
	}
	return path, restart
}

type proxy struct {
	mu     sync.Mutex
	target string
	conns  map[net.Conn]struct{}
}

func (p *proxy) serve(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		target := p.target
		p.mu.Unlock()
		upstream, err := net.Dial("unix", target)
		if err != nil {
			_ = conn.Close()
			continue
		}
		p.track(conn, upstream)
		go func() { _, _ = io.Copy(upstream, conn); p.drop(conn, upstream) }()
		go func() { _, _ = io.Copy(conn, upstream); p.drop(conn, upstream) }()
	}
}

func (p *proxy) track(conns ...net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, conn := range conns {
		p.conns[conn] = struct{}{}
	}
}

func (p *proxy) drop(conns ...net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
		delete(p.conns, conn)
	}
}

func (p *proxy) dropAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for conn := range p.conns {
		_ = conn.Close()
		delete(p.conns, conn)
	}
}

func serveAt(t T, path string) func() {
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
	return srv.Close
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
