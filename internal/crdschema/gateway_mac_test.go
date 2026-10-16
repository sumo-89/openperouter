// SPDX-License-Identifier:Apache-2.0

package crdschema

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
)

// The CRD's own checks of spec.gatewayMAC, as the API server runs them.

func TestGatewayMACSchemaAccepts(t *testing.T) {
	for _, mac := range []string{
		"00:00:5e:00:01:01", // anycast gateway MAC for virtual router ID 1
		"00:00:5E:00:01:FF",
		"02:00:00:00:00:01", // locally administered
		"aa:bb:cc:dd:ee:00",
	} {
		t.Run(mac, func(t *testing.T) {
			obj := gatewayMACL2VNI(map[string]any{"gatewayMAC": mac})
			if errs := Validate(context.Background(), obj, l2vniGVK); len(errs) != 0 {
				t.Errorf("gatewayMAC %q refused: %v", mac, errs)
			}
		})
	}
}

func TestGatewayMACSchemaRefuses(t *testing.T) {
	tests := []struct {
		name      string
		spec      map[string]any
		errSubstr string
	}{
		{"multicast", map[string]any{"gatewayMAC": "01:00:5e:00:00:01"}, "gatewayMAC must be a unicast MAC"},
		{"multicast, upper case", map[string]any{"gatewayMAC": "0F:00:00:00:00:01"}, "gatewayMAC must be a unicast MAC"},
		{"broadcast", map[string]any{"gatewayMAC": "ff:ff:ff:ff:ff:ff"}, "gatewayMAC must be a unicast MAC"},
		{"all zeros", map[string]any{"gatewayMAC": "00:00:00:00:00:00"}, "gatewayMAC must not be all zeros"},
		{"hyphen form", map[string]any{"gatewayMAC": "00-00-5e-00-01-01"}, "spec.gatewayMAC"},
		{"dotted form", map[string]any{"gatewayMAC": "0000.5e00.0101"}, "spec.gatewayMAC"},
		{"too short", map[string]any{"gatewayMAC": "00:00:5e:00:01"}, "spec.gatewayMAC"},
		{"64-bit", map[string]any{"gatewayMAC": "00:00:5e:00:01:01:00:00"}, "spec.gatewayMAC"},
		{"not hex", map[string]any{"gatewayMAC": "00:00:5e:00:01:0g"}, "spec.gatewayMAC"},
		{"without gatewayIPs", map[string]any{"gatewayMAC": "00:00:5e:00:01:01", "gatewayIPs": nil},
			"gatewayMAC cannot be set without gatewayIPs"},
		{"with empty gatewayIPs", map[string]any{"gatewayMAC": "00:00:5e:00:01:01", "gatewayIPs": []any{}},
			"gatewayMAC cannot be set without gatewayIPs"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			obj := gatewayMACL2VNI(tc.spec)
			errs := Validate(context.Background(), obj, l2vniGVK)
			if !hasErrorContaining(errs, tc.errSubstr) {
				t.Errorf("want an error containing %q, got %v", tc.errSubstr, errs)
			}
		})
	}
}

// The field-level "self == oldSelf" that gatewayIPs uses is not evaluated
// when the old object has no value, so it cannot refuse adding or removing
// the field. The spec-level rule covers all three directions.
func TestGatewayMACIsImmutableInTheSchema(t *testing.T) {
	const mac = "00:00:5e:00:01:01"
	tests := []struct {
		name     string
		old, new map[string]any
		refused  bool
	}{
		{"unchanged", map[string]any{"gatewayMAC": mac}, map[string]any{"gatewayMAC": mac}, false},
		{"unchanged unset", map[string]any{}, map[string]any{}, false},
		{"other field changed", map[string]any{"gatewayMAC": mac},
			map[string]any{"gatewayMAC": mac, "vxlanPort": int64(4790)}, false},
		{"set to other", map[string]any{"gatewayMAC": mac}, map[string]any{"gatewayMAC": "00:00:5e:00:01:02"}, true},
		{"unset to set", map[string]any{}, map[string]any{"gatewayMAC": mac}, true},
		{"set to unset", map[string]any{"gatewayMAC": mac}, map[string]any{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateUpdate(t, gatewayMACL2VNI(tc.old), gatewayMACL2VNI(tc.new))
			refused := hasErrorContaining(errs, "gatewayMAC cannot be changed")
			if refused != tc.refused {
				t.Errorf("refused = %v, want %v (errors: %v)", refused, tc.refused, errs)
			}
		})
	}
}

// A fresh load (static configuration) has no old object, so the
// immutability rule must not fire.
func TestGatewayMACImmutabilityIsSkippedWithoutAnOldObject(t *testing.T) {
	errs := Validate(context.Background(), gatewayMACL2VNI(map[string]any{"gatewayMAC": "00:00:5e:00:01:01"}),
		l2vniGVK)
	if hasErrorContaining(errs, "gatewayMAC cannot be changed") {
		t.Errorf("immutability rule fired on a fresh load: %v", errs)
	}
}

// gatewayMACL2VNI returns a valid L2VNI with gateway IPs, with the given spec
// fields set over it; a nil value removes the field.
func gatewayMACL2VNI(fields map[string]any) *unstructured.Unstructured {
	spec := map[string]any{
		"vni":        int64(100),
		"gatewayIPs": []any{"192.0.2.1/24"},
		"routingDomain": map[string]any{
			"type":  "L3VNI",
			"l3vni": map[string]any{"name": "red"},
		},
	}
	for k, v := range fields {
		if v == nil {
			delete(spec, k)
			continue
		}
		spec[k] = v
	}
	return newUnstructured("L2VNI", spec)
}

// validateUpdate runs the CRD's CEL rules, transition rules included, for an
// update from old to obj.
func validateUpdate(t *testing.T, old, obj *unstructured.Unstructured) field.ErrorList {
	t.Helper()
	for _, o := range []*unstructured.Unstructured{old, obj} {
		if err := ApplyDefaults(o, l2vniGVK); err != nil {
			t.Fatalf("ApplyDefaults() returned error: %v", err)
		}
	}
	validator, ok := validators[l2vniGVK]
	if !ok {
		t.Fatal("no CEL validator for L2VNI")
	}
	errs, _ := validator.Validate(context.Background(), field.NewPath(""), schemas[l2vniGVK], obj.Object, old.Object,
		celconfig.RuntimeCELCostBudget)
	return errs
}

func hasErrorContaining(errs field.ErrorList, substr string) bool {
	for _, e := range errs {
		if strings.Contains(e.Error(), substr) {
			return true
		}
	}
	return false
}
