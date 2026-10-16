// SPDX-License-Identifier:Apache-2.0

package webhooks

import (
	"net/http"

	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	healthPath = "/healthz"
	readyPath  = "/readyz"
)

func SetupHealth(mgr ctrl.Manager) {
	mgr.GetWebhookServer().Register(
		healthPath,
		&healthHandler{})

	mgr.GetWebhookServer().Register(
		readyPath,
		&healthHandler{check: CacheSynced})
}

type healthHandler struct {
	check func(*http.Request) error
}

func (h *healthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.check != nil {
		if err := h.check(r); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, err := w.Write([]byte(`{"status": "ok"}`))
	if err != nil {
		Logger.Error("healthcheck", "error when writing reply", err)
	}
}
