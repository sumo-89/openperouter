// SPDX-License-Identifier:Apache-2.0

package webhooks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync/atomic"

	"github.com/openperouter/openperouter/api/v1alpha1"
	"github.com/openperouter/openperouter/internal/conversion"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var (
	Logger                  *slog.Logger
	WebhookClient           client.Reader
	DatapathConfigValidator conversion.DatapathConfigValidator

	cacheInformers atomic.Pointer[[]syncedInformer]
)

type syncedInformer interface {
	HasSynced() bool
}

// SetupCache makes the webhooks read from the manager's cache instead of
// listing from the API server on every admission request, and starts an
// informer for every kind they read.
func SetupCache(ctx context.Context, mgr ctrl.Manager) error {
	var informers []syncedInformer
	for _, obj := range []client.Object{&v1.Node{}, &v1alpha1.L2VNI{}, &v1alpha1.L3VNI{}, &v1alpha1.L3VPN{},
		&v1alpha1.Underlay{}, &v1alpha1.L3Passthrough{}} {
		informer, err := mgr.GetCache().GetInformer(ctx, obj)
		if err != nil {
			return fmt.Errorf("failed to get informer for %T: %w", obj, err)
		}
		informers = append(informers, informer)
	}
	WebhookClient = mgr.GetClient()
	cacheInformers.Store(&informers)
	return nil
}

// CacheSynced is a readiness check. The webhook server starts before the
// manager's caches have synced, and a read from a cache that has not synced
// fails, so a replica is not ready for admission requests until then.
func CacheSynced(_ *http.Request) error {
	informers := cacheInformers.Load()
	if informers == nil {
		return nil
	}
	for _, informer := range *informers {
		if !informer.HasSynced() {
			return errors.New("webhook cache not synced")
		}
	}
	return nil
}

// setupFakeWebhookClient creates a new fake client from the provided slice of client.Object.
func setupFakeWebhookClient(initObjects []client.Object) (client.Reader, error) {
	scheme := runtime.NewScheme()
	if err := v1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("failed to add scheme: %w", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("failed to add scheme: %w", err)
	}

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(initObjects...).
		Build(), nil
}

// objectsFromResources converts the provided slice of CRs to a slice of client.Object.
func objectsFromResources[T client.Object](objs []T) []client.Object {
	output := make([]client.Object, 0, len(objs))
	for _, obj := range objs {
		output = append(output, obj)
	}
	return output
}

// affectedNodes returns the nodes whose configuration a write can change:
// those the written object's nodeSelector selects and, for an update, those
// its old one selected. The other nodes see the same objects before and after
// the write, so validating them can only find failures the write did not
// cause. A nil selector selects every node.
func affectedNodes(ctx context.Context, nodeSelectors ...*metav1.LabelSelector) ([]v1.Node, error) {
	nodes, err := listNodes(ctx)
	if err != nil {
		return nil, err
	}
	selectors := make([]labels.Selector, 0, len(nodeSelectors))
	for _, nodeSelector := range nodeSelectors {
		if nodeSelector == nil {
			return nodes, nil
		}
		selector, err := metav1.LabelSelectorAsSelector(nodeSelector)
		if err != nil {
			// Validating every node reports the invalid selector.
			return nodes, nil
		}
		selectors = append(selectors, selector)
	}
	var affected []v1.Node
	for _, node := range nodes {
		if slices.ContainsFunc(selectors, func(s labels.Selector) bool { return s.Matches(labels.Set(node.Labels)) }) {
			affected = append(affected, node)
		}
	}
	return affected, nil
}

// listNodes reads the Nodes without copying them: the validations only read
// them, and a copy of every Node per admission request is costly with many
// nodes.
func listNodes(ctx context.Context) ([]v1.Node, error) {
	nodeList := &v1.NodeList{}
	if err := WebhookClient.List(ctx, nodeList, client.UnsafeDisableDeepCopy); err != nil {
		return nil, err
	}
	return nodeList.Items, nil
}
