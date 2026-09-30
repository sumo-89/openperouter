// SPDX-License-Identifier:Apache-2.0

package tests

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/openperouter/openperouter/api/v1alpha1"
	"github.com/openperouter/openperouter/e2etests/pkg/config"
	"github.com/openperouter/openperouter/e2etests/pkg/executor"
	"github.com/openperouter/openperouter/e2etests/pkg/infra"
	"github.com/openperouter/openperouter/e2etests/pkg/k8s"
	"github.com/openperouter/openperouter/e2etests/pkg/k8sclient"
	"github.com/openperouter/openperouter/e2etests/pkg/openperouter"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
)

// setHostVLAN is the only place in this test that knows how an L2VNI asks
// for its VLAN on a shared OVS bridge. A nil vlan removes the request.
func setHostVLAN(l2vni *v1alpha1.L2VNI, vlan *int32) {
	l2vni.Spec.HostMaster.OVSBridge.VLANID = vlan
}

// Two L2VNIs share one pre-existing OVS bridge, each on its own VLAN. The
// workloads sit on a second OVS bridge as access ports on those VLANs, joined
// to the shared bridge by a patch-port pair that trunks: the shape
// OVN-Kubernetes localnet builds, where br-int tags a localnet network's
// frames and hands them to the physical bridge over a patch port. The
// workloads are network namespaces; the pings in them run with the router
// pod's ping, through the netns bind mount the router pod already sees.
var _ = Describe("L2VNIs sharing an OVS bridge by VLAN", Ordered, func() {
	const (
		sharedBridge   = "br-ovs-vlan"
		workloadBridge = "br-ovs-wl"
		patchShared    = "patch-vlan-wl"
		patchWorkload  = "patch-wl-vlan"
	)

	type side struct {
		l3        v1alpha1.L3VNI
		l2        v1alpha1.L2VNI
		vlan      int32
		gateway   string // gateway address, without the prefix length
		hostPort  string // the host veth the controller attaches to the shared bridge
		peBridge  string // the L2VNI's bridge in the router namespace
		netns     string // the workload
		mac       string
		cidr      string
		otherCIDR string // an address in the other side's subnet, for the isolation check
	}
	newSide := func(name string, l3vni, l2vni, vlan int32, subnet string) side {
		return side{
			l3: v1alpha1.L3VNI{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: openperouter.Namespace},
				Spec:       v1alpha1.L3VNISpec{VRF: name, VNI: l3vni},
			},
			l2: v1alpha1.L2VNI{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s%d", name, l2vni), Namespace: openperouter.Namespace},
				Spec: v1alpha1.L2VNISpec{
					RoutingDomain: l3vniRoutingDomain(name),
					VNI:           l2vni,
					GatewayIPs:    []string{subnet + ".1/24"},
					HostMaster: &v1alpha1.HostMaster{
						Type: v1alpha1.OVSBridge,
						OVSBridge: &v1alpha1.OVSBridgeConfig{
							Lifecycle: v1alpha1.BridgeLifecycleExternal,
							Name:      new(sharedBridge),
						},
					},
				},
			},
			vlan:     vlan,
			gateway:  subnet + ".1",
			hostPort: fmt.Sprintf("host-e-%d", l2vni),
			peBridge: fmt.Sprintf("br-pe-%d", l2vni),
			netns:    fmt.Sprintf("wl%d", vlan),
			mac:      fmt.Sprintf("02:00:00:00:%02d:10", vlan),
			cidr:     subnet + ".10/24",
		}
	}
	a := newSide("red", 100, 110, 11, "192.171.11")
	b := newSide("blue", 200, 120, 12, "192.171.12")
	a.otherCIDR = "192.171.12.11/24"
	b.otherCIDR = "192.171.11.12/24"

	var (
		cs       clientset.Interface
		nodeName string
		nodeExec executor.Executor
	)

	routerPod := func() *corev1.Pod {
		GinkgoHelper()
		pods, err := openperouter.RouterPodsForNodes(cs, map[string]bool{nodeName: true})
		Expect(err).NotTo(HaveOccurred())
		Expect(pods).To(HaveLen(1))
		return pods[0]
	}
	// inWorkload runs cmd inside a workload netns.
	inWorkload := func(s side, cmd string, args ...string) (string, error) {
		pod := routerPod()
		return executor.ForPodInNamedNetns(pod.Namespace, pod.Name, "frr", "/var/run/netns/"+s.netns).Exec(cmd, args...)
	}
	workloadPings := func(s side, ip string) error {
		_, err := inWorkload(s, "ping", "-c", "1", "-W", "2", ip)
		return err
	}
	gatewayPings := func(s side) error {
		_, err := openperouter.ExecutorForPod(routerPod()).Exec("ping", "-c", "1", "-W", "2", "-I", s.l3.Spec.VRF, discardAddressLength(s.cidr))
		return err
	}
	portTag := func(port string) string {
		out, err := nodeExec.Exec("ovs-vsctl", "--if-exists", "get", "port", port, "tag")
		if err != nil {
			return "error: " + err.Error()
		}
		return strings.TrimSpace(out)
	}
	peBridgeLearned := func(bridge, mac string) bool {
		out, err := nodeExec.Exec("ip", "netns", "exec", openperouter.NamedNetns, "bridge", "fdb", "show", "br", bridge)
		Expect(err).NotTo(HaveOccurred(), out)
		return strings.Contains(out, mac)
	}
	restartRouter := func() {
		GinkgoHelper()
		old := routerPod()
		Expect(cs.CoreV1().Pods(old.Namespace).Delete(context.Background(), old.Name, metav1.DeleteOptions{})).To(Succeed())
		Eventually(func(g Gomega) {
			pods, err := openperouter.RouterPodsForNodes(cs, map[string]bool{nodeName: true})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pods).To(HaveLen(1))
			g.Expect(pods[0].UID).NotTo(Equal(old.UID))
			g.Expect(k8s.PodIsReady(pods[0])).To(BeTrue())
		}, 3*time.Minute, 2*time.Second).Should(Succeed())
	}
	apply := func() {
		GinkgoHelper()
		Expect(Updater.Update(config.Resources{
			L3VNIs: []v1alpha1.L3VNI{a.l3, b.l3},
			L2VNIs: []v1alpha1.L2VNI{a.l2, b.l2},
		})).To(Succeed())
	}
	expectTags := func(tagA, tagB string) {
		GinkgoHelper()
		Eventually(func() string { return portTag(a.hostPort) }, 2*time.Minute, time.Second).Should(Equal(tagA))
		Eventually(func() string { return portTag(b.hostPort) }, 2*time.Minute, time.Second).Should(Equal(tagB))
	}
	expectReachable := func(s side) {
		GinkgoHelper()
		Eventually(func() error { return workloadPings(s, s.gateway) }, time.Minute, time.Second).Should(Succeed(),
			"%s must reach its gateway %s", s.netns, s.gateway)
		Eventually(func() error { return gatewayPings(s) }, time.Minute, time.Second).Should(Succeed(),
			"the gateway in VRF %s must reach %s", s.l3.Spec.VRF, s.netns)
	}

	BeforeAll(func() {
		if HostMode {
			Skip("the workload pings run through the router pod")
		}
		Expect(Updater.CleanAll()).To(Succeed())
		cs = k8sclient.New()

		nodes, err := k8s.GetNodes(cs)
		Expect(err).NotTo(HaveOccurred())
		Expect(nodes).NotTo(BeEmpty())
		nodeName = nodes[0].Name
		nodeExec = executor.ForNode(nodeName)
		for _, s := range []*side{&a, &b} {
			s.l2.Spec.NodeSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/hostname": nodeName}}
			setHostVLAN(&s.l2, new(s.vlan))
		}

		Expect(Updater.Update(config.Resources{Underlays: []v1alpha1.Underlay{infra.Underlay}})).To(Succeed())

		By("creating the shared bridge, the workload bridge, the trunking patch pair and the workloads")
		cmds := [][]string{
			{"ovs-vsctl", "--may-exist", "add-br", sharedBridge},
			{"ovs-vsctl", "--may-exist", "add-br", workloadBridge},
			{"ovs-vsctl",
				"--", "--may-exist", "add-port", sharedBridge, patchShared,
				"--", "set", "interface", patchShared, "type=patch", "options:peer=" + patchWorkload,
				"--", "--may-exist", "add-port", workloadBridge, patchWorkload,
				"--", "set", "interface", patchWorkload, "type=patch", "options:peer=" + patchShared},
		}
		for _, s := range []side{a, b} {
			hostEnd := s.netns + "-h"
			cmds = append(cmds,
				[]string{"ip", "netns", "add", s.netns},
				[]string{"ip", "link", "add", hostEnd, "type", "veth", "peer", "name", "eth0", "netns", s.netns},
				[]string{"ip", "-n", s.netns, "link", "set", "eth0", "address", s.mac},
				[]string{"ip", "-n", s.netns, "addr", "add", s.cidr, "dev", "eth0"},
				[]string{"ip", "-n", s.netns, "link", "set", "eth0", "up"},
				[]string{"ip", "link", "set", hostEnd, "up"},
				[]string{"ovs-vsctl", "add-port", workloadBridge, hostEnd, fmt.Sprintf("tag=%d", s.vlan)},
			)
		}
		for _, c := range cmds {
			out, err := nodeExec.Exec(c[0], c[1:]...)
			Expect(err).NotTo(HaveOccurred(), out)
		}
	})

	AfterAll(func() {
		if HostMode {
			return
		}
		dumpIfFails(cs)
		Expect(Updater.CleanAll()).To(Succeed())
		for _, s := range []side{a, b} {
			_, _ = nodeExec.Exec("ip", "netns", "del", s.netns)
			_, _ = nodeExec.Exec("ovs-vsctl", "--if-exists", "del-port", workloadBridge, s.netns+"-h")
		}
		_, _ = nodeExec.Exec("ovs-vsctl", "--if-exists", "del-br", workloadBridge)
		_, _ = nodeExec.Exec("ovs-vsctl", "--if-exists", "del-br", sharedBridge)
	})

	It("attaches each host veth as an access port on its L2VNI's VLAN", func() {
		apply()
		expectTags("11", "12")
	})

	It("carries each VLAN to its own L2VNI, in both directions", func() {
		expectReachable(a)
		expectReachable(b)
		Expect(peBridgeLearned(a.peBridge, a.mac)).To(BeTrue())
		Expect(peBridgeLearned(b.peBridge, b.mac)).To(BeTrue())
	})

	It("keeps the VLANs apart", func() {
		for _, pair := range [][2]side{{a, b}, {b, a}} {
			from, to := pair[0], pair[1]
			out, err := nodeExec.Exec("ip", "-n", from.netns, "addr", "add", from.otherCIDR, "dev", "eth0")
			Expect(err).NotTo(HaveOccurred(), out)
			By(fmt.Sprintf("pinging %s's gateway from %s, from an address in %s's subnet", to.l2.Name, from.netns, to.l2.Name))
			for range 3 {
				_, err := inWorkload(from, "ping", "-c", "1", "-W", "2", "-I", discardAddressLength(from.otherCIDR), to.gateway)
				Expect(err).To(HaveOccurred(), "%s must not reach %s", from.netns, to.gateway)
			}
			Expect(peBridgeLearned(to.peBridge, from.mac)).To(BeFalse(),
				"%s's frames must not reach %s", from.netns, to.peBridge)
			out, err = nodeExec.Exec("ip", "-n", from.netns, "addr", "del", from.otherCIDR, "dev", "eth0")
			Expect(err).NotTo(HaveOccurred(), out)
		}
	})

	It("keeps the VLANs across a router pod restart", func() {
		restartRouter()
		expectTags("11", "12")
		expectReachable(a)
		expectReachable(b)
	})

	It("keeps the VLANs when the router namespace and its veths are re-created", func() {
		Expect(openperouter.DeleteNamedNetns(nodeName)).To(Succeed())
		restartRouter()
		Eventually(func() bool { return openperouter.UnderlayVethsExists(nodeName) }, time.Minute, 2*time.Second).
			Should(BeTrue(), "the underlay veths must be back before the L2VNIs can be")
		Eventually(func() bool {
			return openperouter.IsInterfaceInDefaultNetns(nodeName, a.hostPort) &&
				openperouter.IsInterfaceInDefaultNetns(nodeName, b.hostPort)
		}, 3*time.Minute, time.Second).Should(BeTrue(), "the controller must re-create the host veths")
		expectTags("11", "12")
		expectReachable(a)
		expectReachable(b)
	})

	It("untags the port when the VLAN is removed, and tags it again when restored", func() {
		setHostVLAN(&a.l2, nil)
		apply()
		expectTags("[]", "12")
		Eventually(func() error { return workloadPings(a, a.gateway) }, time.Minute, time.Second).ShouldNot(Succeed(),
			"with its port untagged, %s's VLAN no longer reaches %s", a.netns, a.l2.Name)
		expectReachable(b)

		setHostVLAN(&a.l2, new(a.vlan))
		apply()
		expectTags("11", "12")
		expectReachable(a)
	})
})
