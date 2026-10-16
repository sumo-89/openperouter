# openperouter

![Version: 0.0.0](https://img.shields.io/badge/Version-0.0.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: v0.0.0](https://img.shields.io/badge/AppVersion-v0.0.0-informational?style=flat-square)

An open implementation of a PE router.

**Homepage:** <https://github.com/openperouter/openperouter>

## Source Code

* <https://github.com/openperouter/openperouter>

## Requirements

Kubernetes: `>= 1.19.0-0`

| Repository | Name | Version |
|------------|------|---------|
|  | crds | 0.0.0 |

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| crds.enabled | bool | `true` |  |
| crds.validationFailurePolicy | string | `"Fail"` |  |
| fullnameOverride | string | `""` |  |
| nameOverride | string | `""` |  |
| openperouter.affinity | object | `{}` | Affinity rules for all openperouter pods (router, controller, nodemarker, hostbridge). |
| openperouter.bgpListenLimit | int | `65535` | Maximum number of dynamic BGP sessions accepted via underlay listen ranges (FRR `bgp listen limit`, 1-65535). |
| openperouter.controller.cniCacheDir | string | `"/var/lib/openperouter/cni/cache"` | CNI cache directory for persistent CNI state across pod restarts |
| openperouter.controller.cniPluginDirs | list | `["/opt/openperouter/cni/bin/"]` | CNI plugin binary directories. The default matches the path baked into the controller image. Override only to use externally-provided binaries (e.g. host-mounted). |
| openperouter.controller.healthProbePort | int | `9081` | Health probe port for liveness and readiness checks |
| openperouter.controller.resources | object | `{"requests":{"cpu":"250m","memory":"256Mi"}}` | Resources of the controller container. Without a CPU request the pod is BestEffort and a reconciliation gets almost no CPU on a busy node. |
| openperouter.datapath | string | `"kernel"` | Datapath to use for L3 forwarding. "kernel" uses the standard Linux kernel datapath; "grout" adds a DPDK-accelerated sidecar that runs alongside FRR (FRR's dplane_grout module syncs routes automatically). |
| openperouter.frr.image.pullPolicy | string | `""` |  |
| openperouter.frr.image.repository | string | `"quay.io/openperouter/openperouter"` |  |
| openperouter.frr.image.tag | string | `""` |  |
| openperouter.frr.reloader.resources | object | `{"requests":{"cpu":"100m","memory":"128Mi"}}` | Resources of the reloader container, which applies every configuration change. No memory limit by default: a kill during a reload leaves FRR partially configured until the next reconciliation. |
| openperouter.frr.reloader.vtyshTimeout | string | `""` | Timeout for vtysh commands used in health checks. Increase under heavy VNI load. Defaults to 10s. |
| openperouter.frr.resources | object | `{"requests":{"cpu":"250m","memory":"256Mi"}}` | Resources of the frr container. A CPU request keeps BGP and BFD timely on a busy node. No memory limit by default: an OOM kill restarts FRR. |
| openperouter.grout.image.pullPolicy | string | `""` |  |
| openperouter.grout.image.repository | string | `"quay.io/openperouter/router"` |  |
| openperouter.grout.image.tag | string | `"main-grout"` |  |
| openperouter.grout.resources.limits.cpu | string | `"500m"` |  |
| openperouter.grout.resources.limits.memory | string | `"2Gi"` |  |
| openperouter.grout.resources.requests.cpu | string | `"250m"` |  |
| openperouter.grout.resources.requests.memory | string | `"512Mi"` |  |
| openperouter.grout.testMode | bool | `false` | Run grout in test mode. Test mode requires no hugepages and uses dummy FIB algorithms, which keeps memory usage low at the cost of real longest prefix match lookups. Leave it disabled for a hugepage-backed dataplane, requesting hugepages under grout.resources. |
| openperouter.hostmode | bool | `false` | If true, enables host mode deployment: deploys hostbridge DaemonSet instead of router and controller, and configures nodemarker to run in webhook-only mode |
| openperouter.image.pullPolicy | string | `""` |  |
| openperouter.image.repository | string | `"quay.io/openperouter/router"` |  |
| openperouter.image.tag | string | `""` |  |
| openperouter.labels | object | `{}` |  |
| openperouter.logLevel | string | `"info"` | Controller log level. Must be one of: `debug`, `info`, `warn` or `error`. |
| openperouter.nodeSelector | object | `{}` | Node selector to constrain all openperouter pods (router, controller, nodemarker, hostbridge) to nodes with matching labels. |
| openperouter.nodemarker.resources | object | `{"requests":{"cpu":"100m","memory":"128Mi"}}` | Resources of the nodemarker container. It serves every admission call for the openperouter resources, so a CPU request keeps the webhooks responsive on a busy node. |
| openperouter.ovsRunDir | string | `"/var/run/openvswitch"` | OVS run directory to mount. This is the directory containing the OVS socket. |
| openperouter.ovsSocketPath | string | `""` | OVS database socket path. Defaults to standard OVS location if not specified. |
| openperouter.podAnnotations | object | `{}` |  |
| openperouter.priorityClassName | string | `""` | Priority class for all openperouter pods (router, controller, nodemarker, hostbridge). The router carries the node's traffic and the nodemarker serves the validating webhooks, so use system-node-critical (or system-cluster-critical): the kubelet and the descheduler do not evict pods of these classes for node pressure. A user-defined PriorityClass only orders scheduling preemption. Some clusters admit the system classes only in namespaces with a matching ResourceQuota. |
| openperouter.runtimeClassName | string | `""` |  |
| openperouter.serviceAccounts.annotations | object | `{}` |  |
| openperouter.serviceAccounts.controller.name | string | `""` |  |
| openperouter.serviceAccounts.create | bool | `true` |  |
| openperouter.serviceAccounts.nodemarker.name | string | `""` |  |
| openperouter.serviceAccounts.perouter.name | string | `""` |  |
| openperouter.tolerations | list | `[{"effect":"NoSchedule","key":"node-role.kubernetes.io/master","operator":"Exists"},{"effect":"NoSchedule","key":"node-role.kubernetes.io/control-plane","operator":"Exists"}]` | Tolerations for all openperouter pods (router, controller, nodemarker, hostbridge). Defaults to tolerating control-plane/master node taints so the pods can run there. |
| openperouter.updateStrategy.type | string | `"RollingUpdate"` |  |
| rbac.create | bool | `true` |  |
| webhook.enabled | bool | `true` |  |

----------------------------------------------
Autogenerated from chart metadata using [helm-docs v1.14.2](https://github.com/norwoodj/helm-docs/releases/v1.14.2)
