// SPDX-License-Identifier:Apache-2.0

package webhooks

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"

	"github.com/openperouter/openperouter/api/v1alpha1"
	"github.com/openperouter/openperouter/internal/conversion"
	v1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const (
	l3vpnValidationWebhookPath = "/validate-openperouter-io-v1alpha1-l3vpn"
)

type L3VPNValidator struct {
	client  client.Client
	decoder admission.Decoder
}

func SetupL3VPN(mgr ctrl.Manager) error {
	validator := &L3VPNValidator{
		client:  mgr.GetClient(),
		decoder: admission.NewDecoder(mgr.GetScheme()),
	}

	mgr.GetWebhookServer().Register(
		l3vpnValidationWebhookPath,
		&webhook.Admission{Handler: validator})

	if _, err := mgr.GetCache().GetInformer(context.Background(), &v1alpha1.L3VPN{}); err != nil {
		return fmt.Errorf("failed to get informer for L3VPN: %w", err)
	}
	return nil
}

func (v *L3VPNValidator) Handle(ctx context.Context, req admission.Request) (resp admission.Response) {
	var l3vpn v1alpha1.L3VPN
	var oldL3VPN v1alpha1.L3VPN
	if req.Operation == v1.Delete {
		if err := v.decoder.DecodeRaw(req.OldObject, &l3vpn); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
	}
	if req.Operation != v1.Delete {
		if err := v.decoder.Decode(req, &l3vpn); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
	}
	if req.Operation != v1.Delete && req.OldObject.Size() > 0 {
		if err := v.decoder.DecodeRaw(req.OldObject, &oldL3VPN); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
	}

	switch req.Operation {
	case v1.Create:
		if err := validateL3VPNCreate(ctx, &l3vpn); err != nil {
			return admission.Denied(err.Error())
		}
	case v1.Update:
		if err := validateL3VPNUpdate(ctx, &l3vpn, &oldL3VPN); err != nil {
			return admission.Denied(err.Error())
		}
	case v1.Delete:
		if err := validateL3VPNDelete(&l3vpn); err != nil {
			return admission.Denied(err.Error())
		}
	}
	return admission.Allowed("")
}

func validateL3VPNCreate(ctx context.Context, l3vpn *v1alpha1.L3VPN) error {
	Logger.Debug("webhook l3vpn", "action", "create", "name", l3vpn.Name, "namespace", l3vpn.Namespace)
	defer Logger.Debug("webhook l3vpn", "action", "end create", "name", l3vpn.Name, "namespace", l3vpn.Namespace)

	return validateL3VPN(ctx, l3vpn, l3vpn.Spec.NodeSelector)
}

func validateL3VPNUpdate(ctx context.Context, l3vpn *v1alpha1.L3VPN, oldL3VPN *v1alpha1.L3VPN) error {
	Logger.Debug("webhook l3vpn", "action", "update", "name", l3vpn.Name, "namespace", l3vpn.Namespace)
	defer Logger.Debug("webhook l3vpn", "action", "end update", "name", l3vpn.Name, "namespace", l3vpn.Namespace)

	if !reflect.DeepEqual(localCIDR(oldL3VPN.Spec.HostSession), localCIDR(l3vpn.Spec.HostSession)) {
		return errors.New("localCIDRs cannot be changed")
	}

	return validateL3VPN(ctx, l3vpn, l3vpn.Spec.NodeSelector, oldL3VPN.Spec.NodeSelector)
}

func validateL3VPNDelete(_ *v1alpha1.L3VPN) error {
	return nil
}

// validateL3VPN contains validation logic that is common to the Create and Update operations.
func validateL3VPN(ctx context.Context, l3vpn *v1alpha1.L3VPN, nodeSelectors ...*metav1.LabelSelector) error {
	existingL3VPNs, err := getL3VPNs(ctx)
	if err != nil {
		return err
	}

	toValidate := make([]v1alpha1.L3VPN, 0, len(existingL3VPNs.Items))
	found := false
	for _, existingL3VPN := range existingL3VPNs.Items {
		if existingL3VPN.Name == l3vpn.Name && existingL3VPN.Namespace == l3vpn.Namespace {
			toValidate = append(toValidate, *l3vpn.DeepCopy())
			found = true
			continue
		}
		toValidate = append(toValidate, existingL3VPN)
	}
	if !found {
		toValidate = append(toValidate, *l3vpn.DeepCopy())
	}

	l2vniList, err := getL2VNIs(ctx)
	if err != nil {
		return err
	}

	underlayList, err := getUnderlays(ctx)
	if err != nil {
		return err
	}

	nodes, err := affectedNodes(ctx, nodeSelectors...)
	if err != nil {
		return fmt.Errorf("failed to get existing Node objects when validating Underlay: %w", err)
	}

	if err := conversion.ValidateSRv6ForNodes(nodes, underlayList.Items, toValidate); err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	l3vniList, err := getL3VNIs(ctx)
	if err != nil {
		return err
	}

	if err := conversion.ValidateOverlayResourcesForNodes(
		ctx,
		nodes,
		l2vniList.Items,
		l3vniList.Items,
		toValidate,
	); err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	return nil
}

func getL3VPNs(ctx context.Context) (*v1alpha1.L3VPNList, error) {
	l3vpnList := &v1alpha1.L3VPNList{}
	err := WebhookClient.List(ctx, l3vpnList, &client.ListOptions{})
	if err != nil {
		return nil, errors.Join(err, errors.New("failed to get existing L3VPN objects"))
	}
	return l3vpnList, nil
}
