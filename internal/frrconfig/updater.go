// SPDX-License-Identifier:Apache-2.0

package frrconfig

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
)

// Updater skips the reload when the running FRR already has the rendered
// configuration: frr-reload.py runs twice per reload even when nothing changed.
// The running FRR is named by an identity the caller supplies (the router pod's
// UID and the frr container's restart count); an empty identity never skips.
// Skipping is safe because frr-reload.py runs `vtysh write` after each reload,
// so a daemon restarted inside the container boots with the last applied
// configuration. Drift in the running FRR is no longer corrected until the
// configuration or the identity changes.
type Updater struct {
	configFile string
	reload     func(context.Context, string) error

	mu       sync.Mutex
	applied  string
	identity string
}

func NewUpdater(socketPath, configFile string) *Updater {
	return &Updater{
		configFile: configFile,
		reload:     UpdaterForSocket(socketPath, configFile),
	}
}

// For returns an update function for the router instance named by identity.
func (u *Updater) For(identity string) func(context.Context, string) error {
	return func(ctx context.Context, config string) error {
		return u.Update(ctx, identity, config)
	}
}

func (u *Updater) Update(ctx context.Context, identity, config string) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.alreadyApplied(identity, config) {
		slog.InfoContext(ctx, "frr configuration unchanged, reload skipped", "router", identity)
		return nil
	}
	u.applied, u.identity = "", ""
	if err := u.reload(ctx, config); err != nil {
		return err
	}
	u.applied, u.identity = config, identity
	return nil
}

func UpdaterForSocket(socketPath, configFile string) func(context.Context, string) error {
	return func(ctx context.Context, config string) error {
		updaterClient := func(ctx context.Context) error {
			client := &http.Client{
				Transport: &http.Transport{
					DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
						return net.Dial("unix", socketPath)
					},
				},
			}

			slog.InfoContext(ctx, "updater requesting update", "socket", socketPath)
			defer slog.InfoContext(ctx, "updater update requested")

			res, err := client.Post("http://unix/", "", nil)
			if err != nil {
				return fmt.Errorf("failed to reload against socket %s: %w", socketPath, err)
			}
			defer func() {
				if err := res.Body.Close(); err != nil {
					slog.ErrorContext(ctx, "failed to close res body", "error", err)
				}
			}()
			if res.StatusCode != http.StatusOK {
				return fmt.Errorf("failed to reload against socket %s, status %d", socketPath, res.StatusCode)
			}
			return nil
		}

		slog.InfoContext(ctx, "updater writing frr file", "file", configFile)
		err := os.WriteFile(configFile, []byte(config), 0600)
		if err != nil {
			return fmt.Errorf("failed to write the config to %s", configFile)
		}
		return updaterClient(ctx)
	}
}

// alreadyApplied tells whether the same router instance accepted exactly this
// configuration, and the file still holds it.
func (u *Updater) alreadyApplied(identity, config string) bool {
	if identity == "" {
		return false
	}
	if identity != u.identity || config != u.applied {
		return false
	}
	onDisk, err := os.ReadFile(u.configFile)
	return err == nil && string(onDisk) == config
}
