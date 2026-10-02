// SPDX-License-Identifier:Apache-2.0

//go:build runasroot

package bridgerefresh

import (
	"context"
	"fmt"
	"maps"
	"net"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/openperouter/openperouter/internal/hostnetwork"
	"github.com/openperouter/openperouter/internal/netnamespace"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

var _ = Describe("Sweep", func() {
	const testNSName = "sweeptest"
	nsPath := fmt.Sprintf("/var/run/netns/%s", testNSName)

	BeforeEach(func() {
		createTestNS(testNSName)
		// No IPv6 link-local addresses, so that the links send no MLD and
		// put no multicast entries into the neighbor tables, whose garbage
		// collection limits are shared with the rest of the host.
		inNS(nsPath, func() error {
			return os.WriteFile("/proc/sys/net/ipv6/conf/default/addr_gen_mode", []byte("1"), 0o644)
		})
	})

	AfterEach(func() {
		StopAll()
		cleanTest(testNSName)
	})

	It("pings the STALE neighbors of every bridge in the sweep from one neighbor dump", func() {
		for i, vni := range []int32{301, 302, 303} {
			addPortedBridge(nsPath, vni, fmt.Sprintf("10.30.%d.1/24", i))
		}
		stale301 := net.ParseIP("10.30.0.10")
		stale302 := net.ParseIP("10.30.1.10")
		stale303 := net.ParseIP("10.30.2.10")
		addStaleNeighbor(nsPath, "br-pe-301", stale301, mac(1))
		addReachableNeighbor(nsPath, "br-pe-301", net.ParseIP("10.30.0.11"), mac(2))
		addStaleNeighbor(nsPath, "br-pe-302", stale302, mac(3))
		addRemoteNeighbor(nsPath, "br-pe-302", net.ParseIP("10.30.1.12"), mac(4))
		addStaleNeighbor(nsPath, "br-pe-303", stale303, mac(5)) // not in the sweep

		bridges := refreshersFor(nsPath, 301, 302)
		var found, pinged int
		dumps, _ := countNeighborDumps(func() {
			inNS(nsPath, func() error {
				found, pinged = refreshStaleNeighbors(context.Background(), bridges)
				return nil
			})
		})
		Expect(dumps).To(Equal(1))
		Expect(found).To(Equal(2))
		Expect(pinged).To(Equal(2))

		Eventually(func() int { return neighborState(nsPath, "br-pe-301", stale301) }).
			ShouldNot(Equal(netlink.NUD_STALE), "the ping moves the entry on to DELAY or PROBE")
		Eventually(func() int { return neighborState(nsPath, "br-pe-302", stale302) }).
			ShouldNot(Equal(netlink.NUD_STALE))
		Consistently(func() int { return neighborState(nsPath, "br-pe-303", stale303) }, "1s").
			Should(Equal(netlink.NUD_STALE), "a bridge outside the sweep is not pinged")
	})

	It("refreshes on its own, one dump per tick for all its bridges", func() {
		addPortedBridge(nsPath, 311, "10.31.0.1/24")
		addPortedBridge(nsPath, 312, "10.31.1.1/24")
		stale := net.ParseIP("10.31.1.10")
		addStaleNeighbor(nsPath, "br-pe-312", stale, mac(6))

		const period = 300 * time.Millisecond
		var dumps atomic.Int64
		saved := neighList
		neighList = func(linkIndex, family int) ([]netlink.Neigh, error) {
			dumps.Add(1)
			return saved(linkIndex, family)
		}
		defer func() { neighList = saved }()

		var refreshers []*BridgeRefresher
		for _, vni := range []int32{311, 312} {
			r, err := New(l2vni(nsPath, vni), StartOptions{RefreshPeriod: period})
			Expect(err).NotTo(HaveOccurred())
			r.Start(context.Background())
			refreshers = append(refreshers, r)
		}
		start := time.Now()

		Eventually(func() int { return neighborState(nsPath, "br-pe-312", stale) }, "2s", "50ms").
			ShouldNot(Equal(netlink.NUD_STALE))
		time.Sleep(time.Second)
		for _, r := range refreshers {
			r.Stop()
		}
		elapsed := time.Since(start)

		// Ticks come every 0.8 to 1 period: at least one, and never one per
		// bridge.
		maxTicks := int64(elapsed/(period*4/5)) + 1
		Expect(dumps.Load()).To(BeNumerically(">=", 1))
		Expect(dumps.Load()).To(BeNumerically("<=", maxTicks))
	})

	It("keeps one sweep per namespace and stops it with the last bridge", func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		var refreshers []*BridgeRefresher
		for _, vni := range []int32{321, 322, 323} {
			r, err := New(l2vni(nsPath, vni), StartOptions{RefreshPeriod: time.Hour})
			Expect(err).NotTo(HaveOccurred())
			r.Start(ctx)
			refreshers = append(refreshers, r)
		}
		Expect(sweepSizes()).To(Equal([]int{3}))

		refreshers[0].Stop()
		Expect(sweepSizes()).To(Equal([]int{2}))

		cancel()
		refreshers[1].wg.Wait()
		refreshers[2].wg.Wait()
		Expect(sweepSizes()).To(BeEmpty())
	})

	It("replaces a bridge's refresher in the sweep without dropping the bridge", func() {
		params := l2vni(nsPath, 331)
		Expect(StartForVNI(context.Background(), params)).To(Succeed())
		first := activeRefreshers[331]

		other := "/var/run/netns/sweeptest-other"
		params.TargetNS = other
		Expect(StartForVNI(context.Background(), params)).To(Succeed())
		Expect(activeRefreshers[331]).NotTo(BeIdenticalTo(first))
		Expect(sweepMembers(other)).To(Equal([]string{"br-pe-331"}))
		Expect(sweepMembers(nsPath)).To(BeEmpty())
	})

	Context("at 600 L2VNIs", func() {
		const (
			nBridges   = 600
			staleEvery = 10 // one IPv4 and one IPv6 STALE neighbor on every tenth bridge
			remotes    = 4  // EVPN-learned neighbors per bridge, two per family, as zebra installs them
		)

		It("costs one neighbor dump per tick instead of one per bridge", func() {
			for i := range nBridges {
				vni := int32(1000 + i)
				br := hostnetwork.BridgeName(vni)
				addPortedBridge(nsPath, vni, fmt.Sprintf("10.%d.%d.1/24", 100+i/250, i%250))
				for k := range remotes / 2 {
					addRemoteNeighbor(nsPath, br, net.ParseIP(fmt.Sprintf("10.%d.%d.%d", 100+i/250, i%250, 20+k)), mac(10_000+i*8+k))
					addRemoteNeighbor(nsPath, br, net.ParseIP(fmt.Sprintf("fd00:%x::%x", i, 0x20+k)), mac(10_000+i*8+4+k))
				}
				if i%staleEvery == 0 {
					addStaleNeighbor(nsPath, br, net.ParseIP(fmt.Sprintf("10.%d.%d.100", 100+i/250, i%250)), mac(20_000+i*2))
					addStaleNeighbor(nsPath, br, net.ParseIP(fmt.Sprintf("fd00:%x::100", i)), mac(20_000+i*2+1))
				}
			}
			wantStale := 2 * nBridges / staleEvery

			vnis := make([]int32, nBridges)
			for i := range vnis {
				vnis[i] = int32(1000 + i)
			}
			bridges := refreshersFor(nsPath, vnis...)
			legacy := slices.Collect(maps.Values(bridges))

			for round := range 3 {
				var found, pinged int
				var took time.Duration
				var dumps, rows int
				victim := measureRTNL(func() {
					dumps, rows = countNeighborDumps(func() {
						start := time.Now()
						inNS(nsPath, func() error {
							found, pinged = refreshStaleNeighbors(context.Background(), bridges)
							return nil
						})
						took = time.Since(start)
					})
				})
				report("sweep", round, dumps, rows, found, pinged, took, victim)
				// The dump count is the guard. The neighbor tables' garbage
				// collection is shared with the whole host, so on a busy test
				// host an entry can go between the setup and the tick; the
				// exact STALE handling is the three-bridge spec's.
				Expect(dumps).To(Equal(1))
				Expect(found).To(BeNumerically(">", wantStale/2))

				// Re-arm: the pings moved the STALE entries on.
				restale(nsPath, nBridges, staleEvery)

				victim = measureRTNL(func() {
					dumps, rows = countNeighborDumps(func() {
						start := time.Now()
						found, pinged = legacyTick(nsPath, legacy)
						took = time.Since(start)
					})
				})
				report("per-bridge", round, dumps, rows, found, pinged, took, victim)
				Expect(dumps).To(Equal(nBridges), "the copy of the old tick lists the namespace once per bridge")
				Expect(found).To(BeNumerically(">", wantStale/2))

				restale(nsPath, nBridges, staleEvery)
			}
		})
	})
})

// legacyTick is one tick of the refresher as it was before the sweep (v0.3.0
// refresher.go and neighbor.go), kept here to measure against: a goroutine per
// bridge, all released at once because their 30 s tickers started together,
// each entering the namespace, looking its bridge up, listing the neighbors of
// the namespace for its bridge and pinging the STALE ones.
func legacyTick(nsPath string, refreshers []*BridgeRefresher) (found, pinged int) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	release := make(chan struct{})
	for _, r := range refreshers {
		wg.Go(func() {
			<-release
			ns, err := netns.GetFromPath(nsPath)
			if err != nil {
				return
			}
			defer func() { _ = ns.Close() }()
			_ = netnamespace.In(ns, func() error {
				bridge, err := netlink.LinkByName(r.bridgeName)
				if err != nil {
					return err
				}
				neighbors, err := neighList(bridge.Attrs().Index, 0)
				if err != nil {
					return err
				}
				f, p := 0, 0
				for _, neigh := range neighbors {
					if neigh.LinkIndex != bridge.Attrs().Index || !isRefreshCandidate(neigh) {
						continue
					}
					f++
					if r.sendPing(neigh.IP) == nil {
						p++
					}
				}
				mu.Lock()
				found += f
				pinged += p
				mu.Unlock()
				return nil
			})
		})
	}
	close(release)
	wg.Wait()
	return found, pinged
}

type rtnlVictim struct {
	calls    int
	max, p99 time.Duration
}

// measureRTNL runs f while another goroutine asks for a link over and over,
// as ovs-vswitchd does for its statistics. Each request takes RTNL, so how
// long it waits tells how long f's dumps held it.
func measureRTNL(f func()) rtnlVictim {
	stop := make(chan struct{})
	var lat []time.Duration
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			start := time.Now()
			_, _ = netlink.LinkByIndex(1)
			lat = append(lat, time.Since(start))
		}
	}()
	f()
	close(stop)
	<-done
	if len(lat) == 0 {
		return rtnlVictim{}
	}
	slices.Sort(lat)
	return rtnlVictim{calls: len(lat), max: lat[len(lat)-1], p99: lat[len(lat)*99/100]}
}

func report(mode string, round, dumps, rows, found, pinged int, took time.Duration, v rtnlVictim) {
	line := fmt.Sprintf("BRIDGEREFRESH-COST mode=%s round=%d dumps=%d rows=%d stale=%d pinged=%d wall_ms=%.1f rtnl_victim_calls=%d rtnl_victim_max_ms=%.2f rtnl_victim_p99_ms=%.2f",
		mode, round, dumps, rows, found, pinged, ms(took), v.calls, ms(v.max), ms(v.p99))
	fmt.Fprintln(GinkgoWriter, line)
	AddReportEntry(mode, line)
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// countNeighborDumps runs f and returns how many neighbor dumps it took and
// how many entries the kernel sent for them between them. The kernel dumps
// the whole namespace whatever link is asked for (netlink.NeighList filters
// in user space), so the wrapper dumps everything itself, counts it, and
// filters the way NeighList would.
func countNeighborDumps(f func()) (dumps, rows int) {
	saved := neighList
	defer func() { neighList = saved }()
	var mu sync.Mutex
	neighList = func(linkIndex, family int) ([]netlink.Neigh, error) {
		all, err := saved(0, family)
		mu.Lock()
		dumps++
		rows += len(all)
		mu.Unlock()
		if linkIndex == 0 {
			return all, err
		}
		var neighbors []netlink.Neigh
		for _, n := range all {
			if n.LinkIndex == linkIndex {
				neighbors = append(neighbors, n)
			}
		}
		return neighbors, err
	}
	f()
	return dumps, rows
}

func inNS(nsPath string, f func() error) {
	GinkgoHelper()
	ns, err := netns.GetFromPath(nsPath)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = ns.Close() }()
	Expect(netnamespace.In(ns, f)).To(Succeed())
}

func l2vni(nsPath string, vni int32) hostnetwork.L2VNIParams {
	return hostnetwork.L2VNIParams{VNIParams: hostnetwork.VNIParams{VNI: vni, TargetNS: nsPath}}
}

func refreshersFor(nsPath string, vnis ...int32) map[string]*BridgeRefresher {
	GinkgoHelper()
	bridges := map[string]*BridgeRefresher{}
	for _, vni := range vnis {
		r, err := New(l2vni(nsPath, vni), StartOptions{})
		Expect(err).NotTo(HaveOccurred())
		bridges[r.bridgeName] = r
	}
	return bridges
}

// addPortedBridge creates br-pe-<vni> with an address and a dummy port, so the
// bridge has carrier and a ping really leaves through it.
func addPortedBridge(nsPath string, vni int32, cidr string) {
	GinkgoHelper()
	br := hostnetwork.BridgeName(vni)
	createTestBridge(nsPath, br)
	addIPToBridge(nsPath, br, cidr)
	inNS(nsPath, func() error {
		bridge, err := netlink.LinkByName(br)
		if err != nil {
			return err
		}
		port := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: fmt.Sprintf("p%d", vni), MasterIndex: bridge.Attrs().Index}}
		if err := netlink.LinkAdd(port); err != nil {
			return err
		}
		return netlink.LinkSetUp(port)
	})
}

// addRemoteNeighbor adds a neighbor the way zebra installs an EVPN-learned
// MAC/IP on an L2VNI bridge: NOARP and externally learned.
func addRemoteNeighbor(nsPath, bridgeName string, ip net.IP, hw net.HardwareAddr) {
	GinkgoHelper()
	inNS(nsPath, func() error {
		bridge, err := netlink.LinkByName(bridgeName)
		if err != nil {
			return err
		}
		return netlink.NeighAdd(&netlink.Neigh{
			LinkIndex:    bridge.Attrs().Index,
			State:        netlink.NUD_NOARP,
			Flags:        netlink.NTF_EXT_LEARNED,
			IP:           ip,
			HardwareAddr: hw,
		})
	})
}

// restale puts the STALE neighbors of the 600-bridge spec back into STALE.
func restale(nsPath string, nBridges, every int) {
	GinkgoHelper()
	inNS(nsPath, func() error {
		for i := 0; i < nBridges; i += every {
			bridge, err := netlink.LinkByName(hostnetwork.BridgeName(int32(1000 + i)))
			if err != nil {
				return err
			}
			for j, ip := range []string{fmt.Sprintf("10.%d.%d.100", 100+i/250, i%250), fmt.Sprintf("fd00:%x::100", i)} {
				if err := netlink.NeighSet(&netlink.Neigh{
					LinkIndex: bridge.Attrs().Index, State: netlink.NUD_STALE,
					IP: net.ParseIP(ip), HardwareAddr: mac(20_000 + i*2 + j),
				}); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func neighborState(nsPath, bridgeName string, ip net.IP) int {
	GinkgoHelper()
	state := -1
	inNS(nsPath, func() error {
		bridge, err := netlink.LinkByName(bridgeName)
		if err != nil {
			return err
		}
		neighbors, err := netlink.NeighList(bridge.Attrs().Index, 0)
		if err != nil {
			return err
		}
		for _, n := range neighbors {
			if n.LinkIndex == bridge.Attrs().Index && n.IP.Equal(ip) {
				state = n.State
			}
		}
		return nil
	})
	return state
}

func mac(i int) net.HardwareAddr {
	return net.HardwareAddr{0x02, 0x00, byte(i >> 24), byte(i >> 16), byte(i >> 8), byte(i)}
}

func sweepSizes() []int {
	sweepsMu.Lock()
	defer sweepsMu.Unlock()
	var sizes []int
	for _, s := range sweeps {
		sizes = append(sizes, len(s.members))
	}
	return sizes
}

func sweepMembers(nsPath string) []string {
	sweepsMu.Lock()
	defer sweepsMu.Unlock()
	var names []string
	for key, s := range sweeps {
		if key.namespace != nsPath {
			continue
		}
		for name := range s.members {
			names = append(names, name)
		}
	}
	return names
}
