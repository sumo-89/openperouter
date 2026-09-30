// SPDX-License-Identifier:Apache-2.0

package hostnetwork

import (
	"maps"
	"slices"
	"testing"

	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/openperouter/openperouter/internal/ovsmodel"
)

func TestReconcilePortVLAN(t *testing.T) {
	trunk := ovsmodel.PortVLANModeTrunk
	access := ovsmodel.PortVLANModeAccess
	tests := []struct {
		name        string
		tag         *int
		mode        *ovsmodel.PortVLANMode
		externalIDs map[string]string
		vlanID      *int32
		wantTag     *int
		wantMode    *ovsmodel.PortVLANMode
		wantIDs     map[string]string
	}{
		{
			name:        "external tag without owner key, vlanID unset: untouched",
			tag:         new(7),
			externalIDs: map[string]string{"other": "x"},
			wantTag:     new(7),
			wantIDs:     map[string]string{"other": "x"},
		},
		{
			name:        "owned tag changed by someone else, vlanID unset: tag kept, owner key dropped",
			tag:         new(12),
			externalIDs: map[string]string{portVLANOwnerKey: "11", "other": "x"},
			wantTag:     new(12),
			wantIDs:     map[string]string{"other": "x"},
		},
		{
			name:        "owned tag cleared by someone else, vlanID unset: owner key dropped",
			externalIDs: map[string]string{portVLANOwnerKey: "11"},
			wantIDs:     map[string]string{},
		},
		{
			name:        "owned tag, vlanID unset: cleared",
			tag:         new(11),
			externalIDs: map[string]string{portVLANOwnerKey: "11", "other": "x"},
			wantIDs:     map[string]string{"other": "x"},
		},
		{
			name:    "untagged port, vlanID set: tagged and owned, mode left empty",
			vlanID:  new(int32(11)),
			wantTag: new(11),
			wantIDs: map[string]string{portVLANOwnerKey: "11"},
		},
		{
			name:        "external tag, vlanID set: taken over",
			tag:         new(7),
			externalIDs: map[string]string{"other": "x"},
			vlanID:      new(int32(11)),
			wantTag:     new(11),
			wantIDs:     map[string]string{portVLANOwnerKey: "11", "other": "x"},
		},
		{
			name:        "owned tag, vlanID changed: re-tagged",
			tag:         new(11),
			externalIDs: map[string]string{portVLANOwnerKey: "11"},
			vlanID:      new(int32(12)),
			wantTag:     new(12),
			wantIDs:     map[string]string{portVLANOwnerKey: "12"},
		},
		{
			name:     "explicit access mode, vlanID set: mode kept, nothing recorded",
			mode:     &access,
			vlanID:   new(int32(11)),
			wantTag:  new(11),
			wantMode: &access,
			wantIDs:  map[string]string{portVLANOwnerKey: "11"},
		},
		{
			name:     "trunk port, vlanID set: made access, trunk recorded",
			mode:     &trunk,
			vlanID:   new(int32(11)),
			wantTag:  new(11),
			wantMode: &access,
			wantIDs:  map[string]string{portVLANOwnerKey: "11", portVLANModeKey: "trunk"},
		},
		{
			name:        "taken-over trunk port, vlanID changed: recorded mode kept",
			tag:         new(11),
			mode:        &access,
			externalIDs: map[string]string{portVLANOwnerKey: "11", portVLANModeKey: "trunk"},
			vlanID:      new(int32(12)),
			wantTag:     new(12),
			wantMode:    &access,
			wantIDs:     map[string]string{portVLANOwnerKey: "12", portVLANModeKey: "trunk"},
		},
		{
			name:        "taken-over trunk port, vlanID unset: tag cleared, trunk restored",
			tag:         new(11),
			mode:        &access,
			externalIDs: map[string]string{portVLANOwnerKey: "11", portVLANModeKey: "trunk"},
			wantMode:    &trunk,
			wantIDs:     map[string]string{},
		},
		{
			name:        "taken-over trunk port, mode changed by someone else, vlanID unset: mode kept",
			tag:         new(11),
			mode:        new(ovsmodel.PortVLANModeNativeTagged),
			externalIDs: map[string]string{portVLANOwnerKey: "11", portVLANModeKey: "trunk"},
			wantMode:    new(ovsmodel.PortVLANModeNativeTagged),
			wantIDs:     map[string]string{},
		},
		{
			name:        "taken-over trunk port re-tagged by someone else, vlanID unset: tag and mode kept",
			tag:         new(13),
			mode:        &access,
			externalIDs: map[string]string{portVLANOwnerKey: "11", portVLANModeKey: "trunk"},
			wantTag:     new(13),
			wantMode:    &access,
			wantIDs:     map[string]string{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			port := &ovsmodel.Port{Tag: tc.tag, VLANMode: tc.mode, ExternalIDs: maps.Clone(tc.externalIDs)}
			reconcilePortVLAN(port, tc.vlanID)
			if !equalTag(port.Tag, tc.wantTag) {
				t.Errorf("tag = %v, want %v", deref(port.Tag), deref(tc.wantTag))
			}
			if !equalMode(port.VLANMode, tc.wantMode) {
				t.Errorf("vlan_mode = %v, want %v", deref(port.VLANMode), deref(tc.wantMode))
			}
			if len(port.ExternalIDs) != 0 || len(tc.wantIDs) != 0 {
				if !maps.Equal(port.ExternalIDs, tc.wantIDs) {
					t.Errorf("external_ids = %v, want %v", port.ExternalIDs, tc.wantIDs)
				}
			}
		})
	}
}

func TestControllerKeyChanges(t *testing.T) {
	tests := []struct {
		name       string
		observed   map[string]string
		desired    map[string]string
		wantDelete []string
		wantInsert map[string]string
	}{
		{
			name:     "no change",
			observed: map[string]string{portVLANOwnerKey: "11", "other": "x"},
			desired:  map[string]string{portVLANOwnerKey: "11", "other": "x"},
		},
		{
			name:       "added",
			observed:   map[string]string{"other": "x"},
			desired:    map[string]string{portVLANOwnerKey: "11", portVLANModeKey: "trunk", "other": "x"},
			wantInsert: map[string]string{portVLANOwnerKey: "11", portVLANModeKey: "trunk"},
		},
		{
			name:       "changed value is delete plus insert",
			observed:   map[string]string{portVLANOwnerKey: "11"},
			desired:    map[string]string{portVLANOwnerKey: "12"},
			wantDelete: []string{portVLANOwnerKey},
			wantInsert: map[string]string{portVLANOwnerKey: "12"},
		},
		{
			name:       "removed; other keys never appear",
			observed:   map[string]string{portVLANOwnerKey: "11", portVLANModeKey: "trunk", "other": "x"},
			desired:    map[string]string{"other": "y"},
			wantDelete: []string{portVLANOwnerKey, portVLANModeKey},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotDelete, gotInsert := controllerKeyChanges(tc.observed, tc.desired)
			if !slices.Equal(gotDelete, tc.wantDelete) {
				t.Errorf("delete = %v, want %v", gotDelete, tc.wantDelete)
			}
			if !maps.Equal(gotInsert, tc.wantInsert) {
				t.Errorf("insert = %v, want %v", gotInsert, tc.wantInsert)
			}
		})
	}
}

func TestWaitTimedOut(t *testing.T) {
	tests := []struct {
		name  string
		reply []ovsdb.OperationResult
		want  bool
	}{
		{"wait timed out", []ovsdb.OperationResult{{Error: "timed out"}, {}}, true},
		{"other wait error", []ovsdb.OperationResult{{Error: "syntax error", Details: "bad column"}}, false},
		{"success", []ovsdb.OperationResult{{}, {Count: 1}}, false},
		{"no reply", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := waitTimedOut(tc.reply); got != tc.want {
				t.Fatalf("waitTimedOut = %v, want %v", got, tc.want)
			}
		})
	}
}

func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}
