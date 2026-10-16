// SPDX-License-Identifier:Apache-2.0

package tests

import (
	"context"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/openperouter/openperouter/api/v1alpha1"
	"github.com/openperouter/openperouter/e2etests/pkg/config"
	"github.com/openperouter/openperouter/e2etests/pkg/executor"
	"github.com/openperouter/openperouter/e2etests/pkg/infra"
	"github.com/openperouter/openperouter/e2etests/pkg/k8sclient"
	"github.com/openperouter/openperouter/e2etests/pkg/openperouter"
	"github.com/openperouter/openperouter/e2etests/pkg/validate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
)

// When zebra dies the other daemons keep running, and the stack must come back
// whole without a container restart: zebra mapping the VRF to its L3VNI, the
// session to the leaf Established, the pod Ready.
var _ = Describe("FRR daemon death", Ordered, func() {
	var cs clientset.Interface

	vniRed := v1alpha1.L3VNI{
		ObjectMeta: metav1.ObjectMeta{Name: "red", Namespace: openperouter.Namespace},
		Spec:       v1alpha1.L3VNISpec{VRF: "red", VNI: 100},
	}

	BeforeAll(func() {
		if HostMode {
			Skip("the frr container's processes are not visible from the node in host mode")
		}
		Expect(Updater.CleanAll()).To(Succeed())

		cs = k8sclient.New()
		Eventually(func() error {
			routers, err := openperouter.Get(cs, HostMode)
			if err != nil {
				return err
			}
			return openperouter.AreReady(routers)
		}, 2*time.Minute, time.Second).ShouldNot(HaveOccurred())

		Expect(Updater.Update(config.Resources{
			Underlays: []v1alpha1.Underlay{infra.Underlay},
			L3VNIs:    []v1alpha1.L3VNI{vniRed},
		})).To(Succeed())
	})

	AfterAll(func() {
		if HostMode {
			return
		}
		Expect(Updater.CleanAll()).To(Succeed())
	})

	It("heals in place after zebra dies", func() {
		const daemon = "zebra"
		routerPods, err := openperouter.RouterPodsForNodes(cs, allNodes(cs))
		Expect(err).NotTo(HaveOccurred())
		Expect(routerPods).NotTo(BeEmpty())
		routerPod := routerPods[0]
		nodeName := routerPod.Spec.NodeName
		node := executor.ForContainer(nodeName)
		frrExec := executor.ForPod(openperouter.Namespace, routerPod.Name, "frr")

		By("waiting for zebra to map red to VNI 100 before the kill")
		Eventually(func() string {
			out, _ := frrExec.Exec("vtysh", "-c", "show vrf vni json")
			return out
		}, 2*time.Minute, time.Second).Should(SatisfyAll(ContainSubstring(`"vrf":"red"`), ContainSubstring(`"vni":100`)))

		restartsBefore := frrRestartCount(cs, routerPod.Name)
		oldPid := daemonPid(node, daemon)
		Expect(oldPid).NotTo(BeEmpty(), "%s is not running on %s", daemon, nodeName)

		By("killing " + daemon + " with SIGKILL from the node")
		Expect(node.Exec("kill", "-9", oldPid)).Error().ToNot(HaveOccurred())

		By("waiting for the whole stack to come back")
		Eventually(func(g Gomega) {
			pid := daemonPid(node, daemon)
			g.Expect(pid).NotTo(BeEmpty(), "%s not running again", daemon)
			g.Expect(pid).NotTo(Equal(oldPid))
			g.Expect(frrExec.Exec("vtysh", "-c", "show vrf vni json")).To(
				SatisfyAll(ContainSubstring(`"vrf":"red"`), ContainSubstring(`"vni":100`)))
			g.Expect(frrExec.Exec("vtysh", "-c", "show evpn vni 100")).To(ContainSubstring("Type: L3"))
			routers, err := openperouter.Get(cs, HostMode)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(openperouter.AreReady(routers)).To(Succeed())
		}, 4*time.Minute, 2*time.Second).Should(Succeed())

		neighborIP, err := infra.NeighborIP(infra.KindLeaf, nodeName)
		Expect(err).NotTo(HaveOccurred())
		validate.SessionWithNeighbor(
			executor.ForContainer(infra.KindLeaf),
			validate.SessionParameters{
				FromName:    infra.KindLeaf,
				ToName:      nodeName,
				NeighborIP:  neighborIP,
				Established: Established,
			},
		)

		By("checking that the frr container healed in place, without a container restart")
		Expect(frrRestartCount(cs, routerPod.Name)).To(Equal(restartsBefore))
	})
})

// daemonPid returns the pid of the named FRR daemon as the kind node sees it,
// or "" when it is not running. pgrep -x does not match the FRR daemons in the
// router image; comm does.
func daemonPid(node executor.Executor, daemon string) string {
	out, _ := node.Exec("sh", "-c",
		`for p in /proc/[0-9]*; do [ "$(cat $p/comm 2>/dev/null)" = "$1" ] && echo ${p#/proc/}; done`, "_", daemon)
	return strings.TrimSpace(out)
}

func frrRestartCount(cs clientset.Interface, podName string) int32 {
	GinkgoHelper()
	pod, err := cs.CoreV1().Pods(openperouter.Namespace).Get(context.Background(), podName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	for _, s := range pod.Status.ContainerStatuses {
		if s.Name == "frr" {
			return s.RestartCount
		}
	}
	Fail("no frr container in " + podName)
	return 0
}
