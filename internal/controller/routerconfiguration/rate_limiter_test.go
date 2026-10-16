// SPDX-License-Identifier:Apache-2.0

package routerconfiguration

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
)

// With one request per node, the backoff of that request is how long a node
// waits to retry a failed pass. It must stay bounded.
func TestNodeRateLimiterCapsTheBackoff(t *testing.T) {
	limiter := nodeRateLimiter()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "node-1"}}

	const want = 30 * time.Second
	var last time.Duration
	for range 40 {
		last = limiter.When(req)
		if last > want {
			t.Fatalf("backoff %v exceeds %v", last, want)
		}
	}
	if last != want {
		t.Fatalf("after many failures the backoff should sit at %v, got %v", want, last)
	}

	limiter.Forget(req)
	if got := limiter.When(req); got >= time.Second {
		t.Fatalf("a request that succeeded must start over, got %v", got)
	}
}
