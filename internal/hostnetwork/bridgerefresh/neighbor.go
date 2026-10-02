// SPDX-License-Identifier:Apache-2.0

package bridgerefresh

import (
	"errors"
	"fmt"

	"github.com/vishvananda/netlink"
)

// neighList is netlink.NeighList. Whatever link it is given, it dumps every
// neighbor of the current namespace and filters in user space, so one call
// costs in proportion to the namespace, under RTNL. It is a variable so that
// tests can count the dumps a sweep takes.
var neighList = netlink.NeighList

// linkByIndex is netlink.LinkByIndex, a single-link request; a variable for
// the same reason as neighList.
var linkByIndex = netlink.LinkByIndex

// listStaleNeighbors returns all STALE neighbors (IPv4 and IPv6) on the bridge.
// These neighbors are about to be garbage collected and need pings
// to refresh them and prevent EVPN Type-2 route withdrawal.
func (r *BridgeRefresher) listStaleNeighbors() ([]netlink.Neigh, error) {
	if _, err := netlink.LinkByName(r.bridgeName); err != nil {
		return nil, fmt.Errorf("failed to get bridge %s: %w", r.bridgeName, err)
	}

	stale, err := collectStaleNeighbors(map[string]bool{r.bridgeName: true})
	if err != nil && stale == nil {
		return nil, err
	}
	return stale[r.bridgeName], nil
}

// collectStaleNeighbors returns the STALE neighbors of the given bridges in
// the current namespace, by bridge name, from a single neighbor dump of the
// namespace. Only links that carry a candidate are looked up, one by one.
//
// An interrupted dump returns what it got together with the error: a
// neighbor it missed is pinged on the next tick, and pinging the ones it got
// is no worse than pinging them a tick late. A larger namespace makes an
// interrupted dump more likely, and treating it as a failure would skip the
// refresh of every bridge until a dump happens to run undisturbed.
func collectStaleNeighbors(bridges map[string]bool) (map[string][]netlink.Neigh, error) {
	neighbors, err := neighList(0, 0)
	if err != nil && !errors.Is(err, netlink.ErrDumpInterrupted) {
		return nil, fmt.Errorf("failed to list neighbors: %w", err)
	}
	if err != nil {
		err = fmt.Errorf("failed to list neighbors: %w", err)
	}

	stale := map[string][]netlink.Neigh{}
	ours := map[int]string{} // link index -> bridge name, "" for links that are not one of the bridges
	for _, neigh := range neighbors {
		if !isRefreshCandidate(neigh) {
			continue
		}
		name, seen := ours[neigh.LinkIndex]
		if !seen {
			if link, lerr := linkByIndex(neigh.LinkIndex); lerr == nil && bridges[link.Attrs().Name] {
				name = link.Attrs().Name
			}
			ours[neigh.LinkIndex] = name
		}
		if name == "" {
			continue
		}
		stale[name] = append(stale[name], neigh)
	}

	return stale, err
}

// isRefreshCandidate tells whether a neighbor needs a ping.
func isRefreshCandidate(neigh netlink.Neigh) bool {
	// Only include STALE neighbors that need refreshing
	if neigh.State&netlink.NUD_STALE == 0 {
		return false
	}

	// Skip entries without MAC address as they won't generate type 2 routes
	if len(neigh.HardwareAddr) == 0 {
		return false
	}

	// Skip entries without IP address
	if neigh.IP == nil {
		return false
	}

	// Skip link-local addresses — they are not advertised as EVPN
	// Type-2 routes, and the bridge typically has no IPv6 address
	// to send ICMPv6 pings from.
	if neigh.IP.IsLinkLocalUnicast() {
		return false
	}

	return true
}
