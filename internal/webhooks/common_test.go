// SPDX-License-Identifier:Apache-2.0

package webhooks

import (
	"net/http/httptest"
	"testing"
)

func TestCacheSynced(t *testing.T) {
	tests := []struct {
		name      string
		informers *[]syncedInformer
		wantReady bool
	}{
		{name: "webhooks not set up", informers: nil, wantReady: true},
		{name: "every informer synced", informers: &[]syncedInformer{fakeInformer(true), fakeInformer(true)},
			wantReady: true},
		{name: "one informer not synced", informers: &[]syncedInformer{fakeInformer(true), fakeInformer(false)},
			wantReady: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			original := cacheInformers.Load()
			defer cacheInformers.Store(original)
			cacheInformers.Store(tc.informers)

			err := CacheSynced(httptest.NewRequest("GET", readyPath, nil))
			if gotReady := err == nil; gotReady != tc.wantReady {
				t.Errorf("CacheSynced() = %v, want ready %v", err, tc.wantReady)
			}
		})
	}
}

type fakeInformer bool

func (f fakeInformer) HasSynced() bool { return bool(f) }
