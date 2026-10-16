// SPDX-License-Identifier:Apache-2.0

//go:build runasroot

package hostnetwork

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/openperouter/openperouter/internal/netnamespace"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

var _ = Describe("VRF routing tables", func() {
	var testNS netns.NsHandle

	BeforeEach(func() {
		cleanTest(testNSName)
		testNS = createTestNS(testNSName)
		setupLoopback(testNS)
	})

	AfterEach(func() {
		cleanTest(testNSName)
	})

	It("should give every VRF it creates its own routing table", func() {
		l3vnis := tableTestL3VNIs(3)
		for _, p := range l3vnis {
			Expect(SetupL3VNI(context.Background(), p)).To(Succeed())
		}

		tables := map[uint32]string{}
		_ = netnamespace.In(testNS, func() error {
			for _, p := range l3vnis {
				_, vrf := validateVRF(Default, p.VRF)
				Expect(tables).NotTo(HaveKey(vrf.Table), "VRFs %s and %s share table %d", tables[vrf.Table], p.VRF, vrf.Table)
				tables[vrf.Table] = p.VRF
			}
			return nil
		})
	})

	It("should not search for a free routing table when setting up existing VRFs", func() {
		l3vnis := tableTestL3VNIs(3)
		for _, p := range l3vnis {
			Expect(SetupL3VNI(context.Background(), p)).To(Succeed())
		}

		searches := countTableSearches(func() {
			for _, p := range l3vnis {
				Expect(SetupL3VNI(context.Background(), p)).To(Succeed())
			}
		})
		Expect(searches).To(Equal(0))
		_ = netnamespace.In(testNS, func() error {
			for _, p := range l3vnis {
				validateL3VNI(Default, p)
			}
			return nil
		})
	})
})

// countTableSearches counts the link dumps f takes inside the test namespace.
// The underlay MTU lookup lists links too, but through netlink.LinkList
// directly, so only the free routing table search is counted.
func countTableSearches(f func()) int {
	saved := linkList
	defer func() { linkList = saved }()
	n := 0
	linkList = func() ([]netlink.Link, error) {
		n++
		return saved()
	}
	f()
	return n
}

func tableTestL3VNIs(n int) []L3VNIParams {
	res := make([]L3VNIParams, 0, n)
	for i := range n {
		res = append(res, L3VNIParams{
			VNIParams: VNIParams{
				VRF:       fmt.Sprintf("testvrf%d", i),
				TargetNS:  testNSPath(),
				VTEPIP:    "192.170.0.9/32",
				VNI:       int32(10000 + i),
				VXLanPort: new(int32(4789)),
			},
			LinkIPs: &LinkIPs{
				HostIPv4: fmt.Sprintf("192.168.%d.1/32", i),
				NSIPv4:   fmt.Sprintf("192.168.%d.0/32", i),
			},
		})
	}
	return res
}
