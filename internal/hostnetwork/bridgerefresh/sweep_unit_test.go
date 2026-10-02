// SPDX-License-Identifier:Apache-2.0

package bridgerefresh

import (
	"testing"
	"time"
)

func TestNextTickStaysWithinThePeriodAndSpreads(t *testing.T) {
	const period = 30 * time.Second
	minTick, maxTick := period, time.Duration(0)
	for range 10_000 {
		d := nextTick(period)
		if d > period || d < period*4/5 {
			t.Fatalf("nextTick(%v) = %v, want within [%v, %v]", period, d, period*4/5, period)
		}
		minTick, maxTick = min(minTick, d), max(maxTick, d)
	}
	if maxTick-minTick < period/10 {
		t.Fatalf("nextTick spread %v..%v, want the ticks spread over most of a fifth of the period", minTick, maxTick)
	}
}
