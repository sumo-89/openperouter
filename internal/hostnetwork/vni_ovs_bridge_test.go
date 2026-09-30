// SPDX-License-Identifier:Apache-2.0

package hostnetwork

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/openperouter/openperouter/internal/netnamespace"
	"github.com/openperouter/openperouter/internal/ovsmodel"
	libovsclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"k8s.io/utils/ptr"
)

var _ = Describe("L2 VNI configuration with OVS bridges", func() {
	var testNS netns.NsHandle

	BeforeEach(func() {
		cleanTest(testNSName)
		testNS = createTestNS(testNSName)
		setupLoopback(testNS)
	})

	AfterEach(func() {
		cleanTest(testNSName)
	})

	It("should work with a single L2VNI using auto-created OVS bridge", func() {
		params := L2VNIParams{
			VNIParams: VNIParams{
				VRF: "testred", TargetNS: testNSPath(),
				VTEPIP: "192.170.0.9/32", VNI: 100, VXLanPort: new(int32(4789)),
			},
			HostMaster: &HostMaster{Type: OVSBridgeLinkType, AutoCreate: new(true)},
		}

		createVRFInNamespace(testNS, params.VRF)
		err := SetupL2VNI(context.Background(), params)
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			validateL2HostLeg(g, params)
			_ = netnamespace.In(testNS, func() error {
				validateL2VNI(g, params)
				return nil
			})
		}, 30*time.Second, 1*time.Second).Should(Succeed())

		By("removing the VNI")
		err = RemoveNonConfiguredVNIs(testNSPath(), []VNIParams{})
		Expect(err).NotTo(HaveOccurred())
		err = RemoveNonConfiguredVRFs(testNSPath(), map[string]bool{})
		Expect(err).NotTo(HaveOccurred())

		By("checking the VNI and OVS bridge are removed")
		vethNames := vethNamesFromVNI(params.VNI)
		Eventually(func(g Gomega) {
			checkLinkdeleted(g, vethNames.HostSide)
			checkOVSHostBridgeDeleted(g, params)
			_ = netnamespace.In(testNS, func() error {
				validateVNIIsNotConfigured(g, params.VNIParams)
				if params.VRF != "" {
					checkLinkdeleted(g, params.VRF)
				}
				return nil
			})
		}, 30*time.Second, 1*time.Second).Should(Succeed())
	})

	It("should work with a single L2VNI using pre-existing named OVS bridge", func() {
		const bridgeName = "test-ovs-br"
		Expect(createExternalOVSBridge(bridgeName)).To(Succeed(), "must pre-provision an OVS bridge")

		createVRFInNamespace(testNS, "testred")
		params := L2VNIParams{
			VNIParams: VNIParams{
				VRF: "testred", TargetNS: testNSPath(),
				VTEPIP: "192.170.0.9/32", VNI: 100, VXLanPort: new(int32(4789)),
			},
			HostMaster: &HostMaster{Type: OVSBridgeLinkType, Name: new(bridgeName)},
		}

		err := SetupL2VNI(context.Background(), params)
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			validateL2HostLeg(g, params)
			checkOVSBridgeExists(g, bridgeName)
			checkVethAttachedToOVSBridge(g, bridgeName, vethNamesFromVNI(params.VNI).HostSide)
		}, 30*time.Second, 1*time.Second).Should(Succeed())

		By("removing the VNI")
		err = RemoveNonConfiguredVNIs(testNSPath(), []VNIParams{})
		Expect(err).NotTo(HaveOccurred())
		err = RemoveNonConfiguredVRFs(testNSPath(), map[string]bool{})
		Expect(err).NotTo(HaveOccurred())

		By("checking the bridge persists but veth is cleaned up")
		vethNames := vethNamesFromVNI(params.VNI)
		Eventually(func(g Gomega) {
			checkOVSBridgeExists(g, bridgeName) // Bridge should still exist
			checkLinkdeleted(g, vethNames.HostSide)
			checkVethNotAttachedToOVSBridge(g, bridgeName, vethNames.HostSide)
			_ = netnamespace.In(testNS, func() error {
				validateVNIIsNotConfigured(g, params.VNIParams)
				if params.VRF != "" {
					checkLinkdeleted(g, params.VRF)
				}
				return nil
			})
		}, 30*time.Second, 1*time.Second).Should(Succeed())
	})

	It("should work with multiple L2VNIs with different auto-created OVS bridges + cleanup", func() {
		params1 := L2VNIParams{
			VNIParams: VNIParams{
				VRF: "testred", TargetNS: testNSPath(),
				VTEPIP: "192.170.0.9/32", VNI: 100, VXLanPort: new(int32(4789)),
			},
			HostMaster: &HostMaster{Type: OVSBridgeLinkType, AutoCreate: new(true)},
		}

		params2 := L2VNIParams{
			VNIParams: VNIParams{
				VRF: "testgreen", TargetNS: testNSPath(),
				VTEPIP: "192.170.0.9/32", VNI: 101, VXLanPort: new(int32(4789)),
			},
			HostMaster: &HostMaster{Type: OVSBridgeLinkType, AutoCreate: new(true)},
		}

		createVRFInNamespace(testNS, params1.VRF)
		createVRFInNamespace(testNS, params2.VRF)
		err := SetupL2VNI(context.Background(), params1)
		Expect(err).NotTo(HaveOccurred())
		err = SetupL2VNI(context.Background(), params2)
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			validateL2HostLeg(g, params1)
			validateL2HostLeg(g, params2)
		}, 30*time.Second, 1*time.Second).Should(Succeed())

		By("removing VNI 100, keeping VNI 101")
		err = RemoveNonConfiguredVNIs(testNSPath(), []VNIParams{params2.VNIParams})
		Expect(err).NotTo(HaveOccurred())
		err = RemoveNonConfiguredVRFs(testNSPath(), map[string]bool{params2.VRF: true})
		Expect(err).NotTo(HaveOccurred())

		By("checking VNI 100 removed, VNI 101 persists")
		Eventually(func(g Gomega) {
			checkOVSHostBridgeDeleted(g, params1)
			checkOVSBridgeExists(g, hostBridgeName(params2.VNI))
		}, 30*time.Second, 1*time.Second).Should(Succeed())
	})

	It("should be idempotent with OVS bridges", func() {
		params := L2VNIParams{
			VNIParams: VNIParams{
				VRF: "testred", TargetNS: testNSPath(),
				VTEPIP: "192.170.0.9/32", VNI: 100, VXLanPort: new(int32(4789)),
			},
			HostMaster: &HostMaster{Type: OVSBridgeLinkType, AutoCreate: new(true)},
		}

		createVRFInNamespace(testNS, params.VRF)
		err := SetupL2VNI(context.Background(), params)
		Expect(err).NotTo(HaveOccurred())

		By("calling SetupL2VNI a second time")
		err = SetupL2VNI(context.Background(), params)
		Expect(err).NotTo(HaveOccurred(), "second SetupL2VNI should be idempotent")

		Eventually(func(g Gomega) {
			validateL2HostLeg(g, params)
		}, 30*time.Second, 1*time.Second).Should(Succeed())
	})

	It("should configure L2 gateway IP with OVS bridge", func() {
		gwIP := "10.10.100.1/24"
		params := L2VNIParams{
			VNIParams: VNIParams{
				VRF: "testred", TargetNS: testNSPath(),
				VTEPIP: "192.170.0.9/32", VNI: 100, VXLanPort: new(int32(4789)),
			},
			L2GatewayIPs: []string{gwIP},
			HostMaster:   &HostMaster{Type: OVSBridgeLinkType, AutoCreate: new(true)},
		}

		createVRFInNamespace(testNS, params.VRF)
		err := SetupL2VNI(context.Background(), params)
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			validateL2HostLeg(g, params)
			_ = netnamespace.In(testNS, func() error {
				validateL2VNI(g, params)
				return nil
			})
		}, 30*time.Second, 1*time.Second).Should(Succeed())
	})
	It("should attach the veth as an access port for vlanID, and follow changes to it", func() {
		const bridgeName = "test-ovs-vlan1"
		Expect(createExternalOVSBridge(bridgeName)).To(Succeed(), "must pre-provision an OVS bridge")

		createVRFInNamespace(testNS, "testred")
		params := L2VNIParams{
			VNIParams: VNIParams{
				VRF: "testred", TargetNS: testNSPath(),
				VTEPIP: "192.170.0.9/32", VNI: 100, VXLanPort: new(int32(4789)),
			},
			HostMaster: &HostMaster{Type: OVSBridgeLinkType, Name: new(bridgeName), VLANID: new(int32(11))},
		}
		hostVeth := vethNamesFromVNI(params.VNI).HostSide

		Expect(SetupL2VNI(context.Background(), params)).To(Succeed())
		checkVethAttachedToOVSBridge(Default, bridgeName, hostVeth)
		checkOVSPortVLAN(Default, hostVeth, new(11))

		By("changing the VLAN")
		params.HostMaster.VLANID = new(int32(12))
		Expect(SetupL2VNI(context.Background(), params)).To(Succeed())
		checkOVSPortVLAN(Default, hostVeth, new(12))

		By("removing the VLAN")
		params.HostMaster.VLANID = nil
		Expect(SetupL2VNI(context.Background(), params)).To(Succeed())
		checkOVSPortVLAN(Default, hostVeth, nil)
	})

	It("should keep the VLAN when the host veth is re-created", func() {
		const bridgeName = "test-ovs-vlan2"
		Expect(createExternalOVSBridge(bridgeName)).To(Succeed(), "must pre-provision an OVS bridge")

		createVRFInNamespace(testNS, "testred")
		params := L2VNIParams{
			VNIParams: VNIParams{
				VRF: "testred", TargetNS: testNSPath(),
				VTEPIP: "192.170.0.9/32", VNI: 100, VXLanPort: new(int32(4789)),
			},
			HostMaster: &HostMaster{Type: OVSBridgeLinkType, Name: new(bridgeName), VLANID: new(int32(11))},
		}
		hostVeth := vethNamesFromVNI(params.VNI).HostSide

		Expect(SetupL2VNI(context.Background(), params)).To(Succeed())
		checkOVSPortVLAN(Default, hostVeth, new(11))

		By("deleting the host veth")
		link, err := netlink.LinkByName(hostVeth)
		Expect(err).NotTo(HaveOccurred())
		Expect(netlink.LinkDel(link)).To(Succeed())
		checkOVSPortVLAN(Default, hostVeth, new(11))

		By("setting the VNI up again")
		Expect(SetupL2VNI(context.Background(), params)).To(Succeed())
		checkVethAttachedToOVSBridge(Default, bridgeName, hostVeth)
		checkOVSPortVLAN(Default, hostVeth, new(11))
	})

	It("should leave a VLAN set by other means alone while vlanID is unset", func() {
		const bridgeName = "test-ovs-vlan3"
		Expect(createExternalOVSBridge(bridgeName)).To(Succeed(), "must pre-provision an OVS bridge")

		createVRFInNamespace(testNS, "testred")
		params := L2VNIParams{
			VNIParams: VNIParams{
				VRF: "testred", TargetNS: testNSPath(),
				VTEPIP: "192.170.0.9/32", VNI: 100, VXLanPort: new(int32(4789)),
			},
			HostMaster: &HostMaster{Type: OVSBridgeLinkType, Name: new(bridgeName)},
		}
		hostVeth := vethNamesFromVNI(params.VNI).HostSide

		Expect(SetupL2VNI(context.Background(), params)).To(Succeed())
		checkOVSPortVLAN(Default, hostVeth, nil)

		By("tagging the port outside the controller")
		out, err := exec.Command("ovs-vsctl", "set", "port", hostVeth, "tag=7").CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), string(out))
		Expect(SetupL2VNI(context.Background(), params)).To(Succeed())
		tag, owned := ovsPortVLAN(Default, hostVeth)
		Expect(tag).To(Equal(new(7)))
		Expect(owned).To(BeFalse())

		By("setting vlanID, which takes over the port's VLAN")
		params.HostMaster.VLANID = new(int32(11))
		Expect(SetupL2VNI(context.Background(), params)).To(Succeed())
		checkOVSPortVLAN(Default, hostVeth, new(11))
	})
	It("should leave a VLAN alone that someone changed after the controller set it", func() {
		const bridgeName = "test-ovs-vlan4"
		Expect(createExternalOVSBridge(bridgeName)).To(Succeed(), "must pre-provision an OVS bridge")

		createVRFInNamespace(testNS, "testred")
		params := L2VNIParams{
			VNIParams: VNIParams{
				VRF: "testred", TargetNS: testNSPath(),
				VTEPIP: "192.170.0.9/32", VNI: 100, VXLanPort: new(int32(4789)),
			},
			HostMaster: &HostMaster{Type: OVSBridgeLinkType, Name: new(bridgeName), VLANID: new(int32(11))},
		}
		hostVeth := vethNamesFromVNI(params.VNI).HostSide

		Expect(SetupL2VNI(context.Background(), params)).To(Succeed())
		checkOVSPortVLAN(Default, hostVeth, new(11))

		By("re-tagging the port outside the controller")
		out, err := exec.Command("ovs-vsctl", "set", "port", hostVeth, "tag=12").CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), string(out))

		By("removing vlanID")
		params.HostMaster.VLANID = nil
		Expect(SetupL2VNI(context.Background(), params)).To(Succeed())
		tag, owned := ovsPortVLAN(Default, hostVeth)
		Expect(tag).To(Equal(new(12)))
		Expect(owned).To(BeFalse())
	})

	It("should refuse, and not re-tag, a port that is attached to another bridge", func() {
		const firstBridge, secondBridge = "test-ovs-vlan5", "test-ovs-vlan6"
		Expect(createExternalOVSBridge(firstBridge)).To(Succeed(), "must pre-provision an OVS bridge")
		Expect(createExternalOVSBridge(secondBridge)).To(Succeed(), "must pre-provision an OVS bridge")

		createVRFInNamespace(testNS, "testred")
		params := L2VNIParams{
			VNIParams: VNIParams{
				VRF: "testred", TargetNS: testNSPath(),
				VTEPIP: "192.170.0.9/32", VNI: 100, VXLanPort: new(int32(4789)),
			},
			HostMaster: &HostMaster{Type: OVSBridgeLinkType, Name: new(firstBridge), VLANID: new(int32(11))},
		}
		hostVeth := vethNamesFromVNI(params.VNI).HostSide

		Expect(SetupL2VNI(context.Background(), params)).To(Succeed())
		checkVethAttachedToOVSBridge(Default, firstBridge, hostVeth)

		By("asking for the same port on another bridge, with another VLAN")
		params.HostMaster.Name = new(secondBridge)
		params.HostMaster.VLANID = new(int32(12))
		err := SetupL2VNI(context.Background(), params)
		Expect(err).To(MatchError(ContainSubstring(
			fmt.Sprintf("port %q is attached to bridge %q, not to %q", hostVeth, firstBridge, secondBridge))))

		checkVethAttachedToOVSBridge(Default, firstBridge, hostVeth)
		checkVethNotAttachedToOVSBridge(Default, secondBridge, hostVeth)
		checkOVSPortVLAN(Default, hostVeth, new(11))
	})
	It("should make an existing trunk port an access port, and restore trunk when vlanID is removed", func() {
		const bridgeName = "test-ovs-vlan7"
		Expect(createExternalOVSBridge(bridgeName)).To(Succeed(), "must pre-provision an OVS bridge")

		createVRFInNamespace(testNS, "testred")
		params := L2VNIParams{
			VNIParams: VNIParams{
				VRF: "testred", TargetNS: testNSPath(),
				VTEPIP: "192.170.0.9/32", VNI: 100, VXLanPort: new(int32(4789)),
			},
			HostMaster: &HostMaster{Type: OVSBridgeLinkType, Name: new(bridgeName)},
		}
		hostVeth := vethNamesFromVNI(params.VNI).HostSide

		Expect(SetupL2VNI(context.Background(), params)).To(Succeed())
		ovsVsctl("set", "port", hostVeth, "vlan_mode=trunk")

		By("setting vlanID")
		params.HostMaster.VLANID = new(int32(11))
		Expect(SetupL2VNI(context.Background(), params)).To(Succeed())
		checkOVSPortVLAN(Default, hostVeth, new(11))
		Expect(ovsVsctl("get", "port", hostVeth, "vlan_mode")).To(Equal("access"))

		By("removing vlanID")
		params.HostMaster.VLANID = nil
		Expect(SetupL2VNI(context.Background(), params)).To(Succeed())
		checkOVSPortVLAN(Default, hostVeth, nil)
		Expect(ovsVsctl("get", "port", hostVeth, "vlan_mode")).To(Equal("trunk"))
		Expect(ovsVsctl("get", "port", hostVeth, "external_ids")).To(Equal("{}"))
	})

	It("should not write a port another client changed after it was read, and keep that change", func() {
		const bridgeName = "test-ovs-vlan8"
		Expect(createExternalOVSBridge(bridgeName)).To(Succeed(), "must pre-provision an OVS bridge")

		createVRFInNamespace(testNS, "testred")
		params := L2VNIParams{
			VNIParams: VNIParams{
				VRF: "testred", TargetNS: testNSPath(),
				VTEPIP: "192.170.0.9/32", VNI: 100, VXLanPort: new(int32(4789)),
			},
			HostMaster: &HostMaster{Type: OVSBridgeLinkType, Name: new(bridgeName), VLANID: new(int32(11))},
		}
		hostVeth := vethNamesFromVNI(params.VNI).HostSide
		Expect(SetupL2VNI(context.Background(), params)).To(Succeed())

		ctx := context.Background()
		ovs, err := NewOVSClient(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer ovs.Close()
		_, err = ovs.Monitor(ctx, ovs.NewMonitor(libovsclient.WithTable(&ovsmodel.Port{})))
		Expect(err).NotTo(HaveOccurred())
		observed := &ovsmodel.Port{Name: hostVeth}
		Expect(ovs.Get(ctx, observed)).To(Succeed())

		By("another client adding an unrelated key after the port was read")
		ovsVsctl("set", "port", hostVeth, "external_ids:other=b")

		By("releasing the VLAN against the stale read")
		Expect(applyPortVLAN(ctx, ovs, observed.DeepCopy(), nil)).To(MatchError(errPortChanged))
		checkOVSPortVLAN(Default, hostVeth, new(11))
		Expect(ovsVsctl("get", "port", hostVeth, "external_ids:other")).To(Equal("b"))

		By("releasing it again, which re-reads the port")
		Expect(ensurePortVLAN(ctx, ovs, hostVeth, nil)).To(Succeed())
		checkOVSPortVLAN(Default, hostVeth, nil)
		Expect(ovsVsctl("get", "port", hostVeth, "external_ids:other")).To(Equal("b"))
	})
})

func checkOVSBridgeExists(g Gomega, bridgeName string) {
	bridge, err := getOVSBridge(bridgeName)
	g.Expect(err).NotTo(HaveOccurred(), "failed to get OVS bridge %q", bridgeName)
	g.Expect(bridge).NotTo(BeNil())
	g.Expect(bridge.Name).To(Equal(bridgeName))
}

func checkOVSHostBridgeDeleted(g Gomega, params L2VNIParams) {
	g.Expect(params.HostMaster).ToNot(BeNil())
	g.Expect(params.HostMaster.Type).To(Equal(OVSBridgeLinkType))
	g.Expect(ptr.Deref(params.HostMaster.AutoCreate, false)).To(BeTrue())

	hostBridge := hostBridgeName(params.VNI)
	checkOVSBridgeDeleted(g, hostBridge)
}

func checkOVSBridgeDeleted(g Gomega, bridgeName string) {
	_, err := getOVSBridge(bridgeName)
	g.Expect(err).To(HaveOccurred(), "OVS bridge %q should not exist", bridgeName)
}

// checkVethAttachedToOVSBridge validates that a veth is attached to an OVS bridge
func checkVethAttachedToOVSBridge(g Gomega, bridgeName, vethName string) {
	hasPort, err := ovsBridgeHasPort(bridgeName, vethName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(hasPort).To(BeTrue(), "veth %s should be attached to OVS bridge %s", vethName, bridgeName)
}

// getOVSBridge retrieves an OVS bridge by name, returns error if not found
func getOVSBridge(name string) (*ovsmodel.Bridge, error) {
	ctx := context.Background()
	ovs, err := NewOVSClient(ctx)
	if err != nil {
		return nil, err
	}
	defer ovs.Close()

	_, err = ovs.Monitor(ctx, ovs.NewMonitor(libovsclient.WithTable(&ovsmodel.Bridge{})))
	if err != nil {
		return nil, err
	}

	bridge := &ovsmodel.Bridge{Name: name}
	err = ovs.Get(ctx, bridge)
	if err != nil {
		return nil, err
	}
	return bridge, nil
}

// ovsBridgeHasPort checks if a port is attached to an OVS bridge
func ovsBridgeHasPort(bridgeName, portName string) (bool, error) {
	ctx := context.Background()
	ovs, err := NewOVSClient(ctx)
	if err != nil {
		return false, err
	}
	defer ovs.Close()

	_, err = ovs.Monitor(ctx, ovs.NewMonitor(
		libovsclient.WithTable(&ovsmodel.Bridge{}),
		libovsclient.WithTable(&ovsmodel.Port{}),
	))
	if err != nil {
		return false, err
	}

	bridge := &ovsmodel.Bridge{Name: bridgeName}
	err = ovs.Get(ctx, bridge)
	if err != nil {
		return false, err
	}

	port := &ovsmodel.Port{Name: portName}
	err = ovs.Get(ctx, port)
	if err != nil {
		return false, nil // Port doesn't exist
	}

	if slices.Contains(bridge.Ports, port.UUID) {
		return true, nil
	}
	return false, nil
}

// waitForOVSInterface waits for an OVS interface to appear using netlink notifications
func waitForOVSInterface(name string) error {
	if _, err := netlink.LinkByName(name); err == nil {
		return nil
	}

	ch := make(chan netlink.LinkUpdate)
	done := make(chan struct{})
	defer close(done)

	if err := netlink.LinkSubscribe(ch, done); err != nil {
		return fmt.Errorf("failed to subscribe to link updates: %w", err)
	}

	timeout := time.After(5 * time.Second)
	for {
		select {
		case update := <-ch:
			if update.Link.Attrs().Name == name {
				return nil
			}
		case <-timeout:
			return fmt.Errorf("timeout waiting for OVS interface %s to appear", name)
		}
	}
}

// checkVethNotAttachedToOVSBridge validates that a veth port has been removed from an OVS bridge
func checkVethNotAttachedToOVSBridge(g Gomega, bridgeName, vethName string) {
	hasPort, err := ovsBridgeHasPort(bridgeName, vethName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(hasPort).To(BeFalse(), "veth %s should not be attached to OVS bridge %s after cleanup", vethName, bridgeName)
}

// createExternalOVSBridge creates an OVS bridge without the "created-by: openperouter"
// marker, simulating a bridge provisioned by the user externally.
func createExternalOVSBridge(name string) error {
	cmd := exec.Command("ovs-vsctl", "add-br", name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ovs-vsctl add-br %s failed: %s: %w", name, string(out), err)
	}
	return waitForOVSInterface(name)
}

// cleanupOVSBridges removes all test OVS bridges
func cleanupOVSBridges() {
	cmd := exec.Command("ovs-vsctl", "list-br")
	output, err := cmd.Output()
	if err != nil {
		return // OVS not available
	}

	bridges := strings.SplitSeq(strings.TrimSpace(string(output)), "\n")
	for br := range bridges {
		if strings.HasPrefix(br, "br-hs-") || strings.HasPrefix(br, "test-ovs-") {
			_ = exec.Command("ovs-vsctl", "del-br", br).Run()
		}
	}
}

// checkOVSPortVLAN checks that the port is an access port for vlan, carrying
// the controller's owner key, or that it has neither when vlan is nil.
func checkOVSPortVLAN(g Gomega, portName string, vlan *int) {
	tag, owned := ovsPortVLAN(g, portName)
	g.Expect(tag).To(Equal(vlan), "port %s VLAN", portName)
	g.Expect(owned).To(Equal(vlan != nil), "port %s owner key", portName)
}

func ovsPortVLAN(g Gomega, portName string) (*int, bool) {
	ctx := context.Background()
	ovs, err := NewOVSClient(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	defer ovs.Close()

	_, err = ovs.Monitor(ctx, ovs.NewMonitor(libovsclient.WithTable(&ovsmodel.Port{})))
	g.Expect(err).NotTo(HaveOccurred())

	port := &ovsmodel.Port{Name: portName}
	g.Expect(ovs.Get(ctx, port)).To(Succeed())
	_, owned := port.ExternalIDs[portVLANOwnerKey]
	return port.Tag, owned
}

func ovsVsctl(args ...string) string {
	GinkgoHelper()
	out, err := exec.Command("ovs-vsctl", args...).CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), string(out))
	return strings.Trim(strings.TrimSpace(string(out)), `"`)
}
