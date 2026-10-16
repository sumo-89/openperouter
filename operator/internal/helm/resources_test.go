// SPDX-License-Identifier:Apache-2.0

package helm

import (
	"testing"

	. "github.com/onsi/gomega"
	operatorapi "github.com/openperouter/openperouter/operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestParseChartResourceDefaults(t *testing.T) {
	g := NewGomegaWithT(t)
	specs := renderPodSpecs(g, operatorapi.OpenPERouterSpec{})

	want := map[string]v1.ResourceList{
		"frr":        {v1.ResourceCPU: resource.MustParse("250m"), v1.ResourceMemory: resource.MustParse("256Mi")},
		"reloader":   {v1.ResourceCPU: resource.MustParse("100m"), v1.ResourceMemory: resource.MustParse("128Mi")},
		"controller": {v1.ResourceCPU: resource.MustParse("250m"), v1.ResourceMemory: resource.MustParse("256Mi")},
		"nodemarker": {v1.ResourceCPU: resource.MustParse("100m"), v1.ResourceMemory: resource.MustParse("128Mi")},
	}
	containers := map[string]*v1.Container{
		"frr":        findContainer(specs[routerDaemonSetName], "frr"),
		"reloader":   findContainer(specs[routerDaemonSetName], "reloader"),
		"controller": findContainer(specs[controllerDaemonSetName], "controller"),
		"nodemarker": findContainer(specs[nodemarkerDeploymentName], "nodemarker"),
	}
	for name, c := range containers {
		g.Expect(c).ToNot(BeNil(), name)
		g.Expect(equalResourceLists(c.Resources.Requests, want[name])).To(BeTrue(),
			"%s requests: got %v, want %v", name, c.Resources.Requests, want[name])
		g.Expect(c.Resources.Limits).To(BeEmpty(), "%s has limits by default", name)
	}
}

func TestParseChartPriorityClassName(t *testing.T) {
	g := NewGomegaWithT(t)
	specs := renderPodSpecs(g, operatorapi.OpenPERouterSpec{PriorityClassName: new("system-node-critical")})

	for _, name := range []string{routerDaemonSetName, controllerDaemonSetName, nodemarkerDeploymentName} {
		g.Expect(specs).To(HaveKey(name))
		g.Expect(specs[name].PriorityClassName).To(Equal("system-node-critical"), name)
	}
}

func TestParseChartResourceOverrides(t *testing.T) {
	g := NewGomegaWithT(t)
	overridden := v1.ResourceRequirements{
		Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"), v1.ResourceMemory: resource.MustParse("512Mi")},
		Limits:   v1.ResourceList{v1.ResourceMemory: resource.MustParse("2Gi")},
	}
	specs := renderPodSpecs(g, operatorapi.OpenPERouterSpec{
		HealthProbePort: new(int32(9999)),
		Resources: &operatorapi.ContainerResources{
			Controller: &overridden,
			NodeMarker: &overridden,
			Reloader:   &overridden,
		},
	})

	for name, c := range map[string]*v1.Container{
		"controller": findContainer(specs[controllerDaemonSetName], "controller"),
		"nodemarker": findContainer(specs[nodemarkerDeploymentName], "nodemarker"),
		"reloader":   findContainer(specs[routerDaemonSetName], "reloader"),
	} {
		g.Expect(c).ToNot(BeNil(), name)
		g.Expect(equalResourceLists(c.Resources.Requests, overridden.Requests)).To(BeTrue(), "%s requests: %v", name, c.Resources.Requests)
		g.Expect(equalResourceLists(c.Resources.Limits, overridden.Limits)).To(BeTrue(), "%s limits: %v", name, c.Resources.Limits)
	}
	frr := findContainer(specs[routerDaemonSetName], "frr")
	g.Expect(frr).ToNot(BeNil())
	g.Expect(equalResourceLists(frr.Resources.Requests, v1.ResourceList{
		v1.ResourceCPU: resource.MustParse("250m"), v1.ResourceMemory: resource.MustParse("256Mi"),
	})).To(BeTrue(), "frr keeps the chart default: %v", frr.Resources.Requests)

	controller := findContainer(specs[controllerDaemonSetName], "controller")
	g.Expect(controller.Args).To(ContainElement(ContainSubstring("9999")), "the controller's other values are kept")
}

// renderPodSpecs renders the chart through the operator and returns the pod
// specs of the DaemonSets and of the
// nodemarker Deployment, keyed by name.
func renderPodSpecs(g Gomega, spec operatorapi.OpenPERouterSpec) map[string]v1.PodSpec {
	chart, err := NewChart(testChartPath, openperouterChartName, openperouterTestNamespace)
	g.Expect(err).ToNot(HaveOccurred())
	openperouter := &operatorapi.OpenPERouter{
		ObjectMeta: metav1.ObjectMeta{Name: "openperouter", Namespace: openperouterTestNamespace},
		Spec:       spec,
	}
	objs, err := chart.Objects(defaultEnvConfig, openperouter)
	g.Expect(err).ToNot(HaveOccurred())

	specs := map[string]v1.PodSpec{}
	for _, obj := range objs {
		switch obj.GetKind() {
		case daemonSetKind:
			ds := appsv1.DaemonSet{}
			g.Expect(runtime.DefaultUnstructuredConverter.FromUnstructured(obj.UnstructuredContent(), &ds)).To(Succeed())
			specs[obj.GetName()] = ds.Spec.Template.Spec
		case deploymentKind:
			deployment := appsv1.Deployment{}
			g.Expect(runtime.DefaultUnstructuredConverter.FromUnstructured(obj.UnstructuredContent(), &deployment)).To(Succeed())
			specs[obj.GetName()] = deployment.Spec.Template.Spec
		}
	}
	g.Expect(specs).To(HaveKey(routerDaemonSetName))
	g.Expect(specs).To(HaveKey(controllerDaemonSetName))
	g.Expect(specs).To(HaveKey(nodemarkerDeploymentName))
	return specs
}

func findContainer(spec v1.PodSpec, name string) *v1.Container {
	for i := range spec.Containers {
		if spec.Containers[i].Name == name {
			return &spec.Containers[i]
		}
	}
	return nil
}

func equalResourceLists(got, want v1.ResourceList) bool {
	if len(got) != len(want) {
		return false
	}
	for name, qty := range want {
		gotQty, ok := got[name]
		if !ok || gotQty.Cmp(qty) != 0 {
			return false
		}
	}
	return true
}
