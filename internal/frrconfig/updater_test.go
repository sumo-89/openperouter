// SPDX-License-Identifier:Apache-2.0

package frrconfig

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestUpdaterForSocket(t *testing.T) {
	tmpfile, err := os.CreateTemp("", "config")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer func() {
		if err := os.Remove(tmpfile.Name()); err != nil {
			t.Fatalf("failed to remove temp file: %v", err)
		}
	}()

	// Create a temporary socket
	socketPath := "/tmp/test_socket"
	defer func() {
		_ = os.Remove(socketPath)
	}()

	// Create a unix socket server
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("failed to create unix socket: %v", err)
	}
	defer func() {
		_ = listener.Close()
	}()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST request, got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	})

	server := &http.Server{Handler: handler}
	go func() {
		_ = server.Serve(listener)
	}()
	defer func() {
		_ = server.Close()
	}()

	updater := UpdaterForSocket(socketPath, tmpfile.Name())

	err = updater(context.Background(), "test config")
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}

	content, err := os.ReadFile(tmpfile.Name())
	if err != nil {
		t.Fatalf("failed to read temp file: %v", err)
	}
	expectedContent := "test config"
	if string(content) != expectedContent {
		t.Errorf("expected content %q, got %q", expectedContent, string(content))
	}
}

func TestUpdaterSkipsReloadOfAppliedConfig(t *testing.T) {
	dir := socketDir(t)
	socketPath := filepath.Join(dir, "reload.sock")
	configFile := filepath.Join(dir, "frr.conf")
	reloads := serveReloader(t, socketPath, http.StatusOK)
	u := NewUpdater(socketPath, configFile)

	mustUpdate(t, u, "pod-1/0", "config A")
	mustUpdate(t, u, "pod-1/0", "config A")
	if got := reloads.Load(); got != 1 {
		t.Fatalf("an unchanged config must not be reloaded again, got %d reloads", got)
	}

	mustUpdate(t, u, "pod-1/0", "config B")
	if got := reloads.Load(); got != 2 {
		t.Fatalf("a changed config must be reloaded, got %d reloads", got)
	}

	if err := os.WriteFile(configFile, []byte("edited by hand"), 0600); err != nil {
		t.Fatal(err)
	}
	mustUpdate(t, u, "pod-1/0", "config B")
	if got := reloads.Load(); got != 3 {
		t.Fatalf("a config file that changed on disk must be reloaded, got %d reloads", got)
	}
}

// The socket's identity is not the router's: a new pod may get the same inode
// back. Same config, same socket, new router identity must reload.
func TestUpdaterReloadsForANewRouterIdentity(t *testing.T) {
	dir := socketDir(t)
	socketPath := filepath.Join(dir, "reload.sock")
	reloads := serveReloader(t, socketPath, http.StatusOK)
	u := NewUpdater(socketPath, filepath.Join(dir, "frr.conf"))

	mustUpdate(t, u, "pod-1/0", "config A")
	mustUpdate(t, u, "pod-1/1", "config A") // the frr container restarted
	mustUpdate(t, u, "pod-2/0", "config A") // a new router pod
	if got := reloads.Load(); got != 3 {
		t.Fatalf("each new router identity must get the config, got %d reloads", got)
	}
}

func TestUpdaterNeverSkipsWithoutARouterIdentity(t *testing.T) {
	dir := socketDir(t)
	socketPath := filepath.Join(dir, "reload.sock")
	reloads := serveReloader(t, socketPath, http.StatusOK)
	update := NewUpdater(socketPath, filepath.Join(dir, "frr.conf")).For("")

	for range 2 {
		if err := update(context.Background(), "config A"); err != nil {
			t.Fatal(err)
		}
	}
	if got := reloads.Load(); got != 2 {
		t.Fatalf("without a router identity every update must reload, got %d reloads", got)
	}
}

func TestUpdaterRetriesAfterAFailedReload(t *testing.T) {
	dir := socketDir(t)
	socketPath := filepath.Join(dir, "reload.sock")
	reloads := serveReloader(t, socketPath, http.StatusInternalServerError)
	u := NewUpdater(socketPath, filepath.Join(dir, "frr.conf"))

	for range 2 {
		if err := u.Update(context.Background(), "pod-1/0", "config A"); err == nil {
			t.Fatal("expected the failed reload to be reported")
		}
	}
	if got := reloads.Load(); got != 2 {
		t.Fatalf("a config the reloader refused must be retried, got %d reloads", got)
	}
}

func TestUpdaterIsSafeForConcurrentUse(t *testing.T) {
	dir := socketDir(t)
	socketPath := filepath.Join(dir, "reload.sock")
	reloads := serveReloader(t, socketPath, http.StatusOK)
	u := NewUpdater(socketPath, filepath.Join(dir, "frr.conf"))

	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			if err := u.Update(context.Background(), "pod-1/0", "config A"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if got := reloads.Load(); got != 1 {
		t.Fatalf("concurrent updates with one config must reload once, got %d", got)
	}
}

// socketDir is a short temporary directory: t.TempDir can exceed the length
// limit of a unix socket path.
func socketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "frrup")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func mustUpdate(t *testing.T, u *Updater, identity, config string) {
	t.Helper()
	if err := u.Update(context.Background(), identity, config); err != nil {
		t.Fatalf("update %q: %v", config, err)
	}
}

// serveReloader serves a fake reloader on a fresh socket at socketPath and
// counts the reload requests it answers with status.
func serveReloader(t *testing.T, socketPath string, status int) *atomic.Int32 {
	t.Helper()
	_ = os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("failed to listen on %s: %v", socketPath, err)
	}
	var reloads atomic.Int32
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reloads.Add(1)
		w.WriteHeader(status)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return &reloads
}
