// SPDX-License-Identifier:Apache-2.0

package hostnetwork

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"time"

	libovsclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
	"github.com/vishvananda/netlink"

	"github.com/openperouter/openperouter/internal/ovsmodel"
)

const (
	defaultOVSSocketPath = "unix:/var/run/openvswitch/db.sock"

	// portVLANOwnerKey is the port external_ids key recording a VLAN set by
	// the controller, so that dropping vlanID from an L2VNI clears that VLAN
	// without touching one set on the port by other means.
	portVLANOwnerKey = "openperouter-vlan"

	// portVLANModeKey records the vlan_mode a port had before the controller
	// made it an access port, so that dropping vlanID restores it.
	portVLANModeKey = "openperouter-vlan-mode"

	// portVLANAttempts bounds the retries when another OVSDB client changes a
	// port between the controller reading it and writing its VLAN.
	portVLANAttempts = 3
)

// errPortChanged means the port no longer matched what the controller read
// when its VLAN transaction ran, so nothing was written.
var errPortChanged = errors.New("port changed concurrently")

var OVSSocketPath = defaultOVSSocketPath

// NewOVSClient creates a new OVS database client
func NewOVSClient(ctx context.Context) (libovsclient.Client, error) {
	dbModel, err := ovsmodel.FullDatabaseModel()
	if err != nil {
		return nil, fmt.Errorf("failed to build OVS DB model: %w", err)
	}

	ovs, err := libovsclient.NewOVSDBClient(
		dbModel,
		libovsclient.WithEndpoint(OVSSocketPath),
	)

	if err != nil {
		return nil, fmt.Errorf("failed to create OVSDB client: %w", err)
	}
	if err := ovs.Connect(ctx); err != nil {
		return nil, fmt.Errorf("failed to connect to OVSDB: %w", err)
	}
	return ovs, nil
}

// EnsureBridge ensures an OVS bridge exists, creating it if necessary
func EnsureBridge(ctx context.Context, ovs libovsclient.Client, bridgeName string) (string, error) {
	br := &ovsmodel.Bridge{Name: bridgeName}
	err := ovs.Get(ctx, br)
	if err == nil {
		return br.UUID, nil
	}
	if !errors.Is(err, libovsclient.ErrNotFound) {
		return "", fmt.Errorf("failed to check if bridge %q exists: %w", bridgeName, err)
	}

	namedUUID := "new_bridge"
	br = &ovsmodel.Bridge{
		UUID:        namedUUID,
		Name:        bridgeName,
		ExternalIDs: map[string]string{"created-by": "openperouter"},
	}

	insertOp, err := ovs.Create(br)
	if err != nil {
		return "", fmt.Errorf("failed to create bridge insert operation: %w", err)
	}

	ovsRow := &ovsmodel.OpenvSwitch{}
	mutateOp, err := ovs.WhereCache(func(*ovsmodel.OpenvSwitch) bool { return true }).
		Mutate(ovsRow, model.Mutation{
			Field:   &ovsRow.Bridges,
			Mutator: ovsdb.MutateOperationInsert,
			Value:   []string{namedUUID},
		})
	if err != nil {
		return "", fmt.Errorf("failed to create mutate operation: %w", err)
	}

	operations := append(insertOp, mutateOp...)
	reply, err := ovs.Transact(ctx, operations...)
	if err != nil {
		return "", fmt.Errorf("transaction failed: %w", err)
	}

	_, err = ovsdb.CheckOperationResults(reply, operations)
	if err != nil {
		return "", fmt.Errorf("operation failed: %w", err)
	}

	realUUID := reply[0].UUID.GoUUID
	slog.Debug("created OVS bridge", "name", bridgeName, "UUID", realUUID)

	return realUUID, nil
}

func ensureOVSBridgeAndAttach(ctx context.Context, bridgeName, ifaceName string, vlanID *int32) error {
	slog.Info("ensureOVSBridgeAndAttach", "bridge", bridgeName, "interface", ifaceName, "vlanID", vlanID)

	// Verify the interface exists before trying to attach to OVS
	link, err := netlink.LinkByName(ifaceName)
	if err != nil {
		return fmt.Errorf("interface %s does not exist on host (required before attaching to OVS bridge): %w", ifaceName, err)
	}
	slog.Info("interface exists on host", "name", ifaceName, "index", link.Attrs().Index, "type", link.Type())

	ovs, err := NewOVSClient(ctx)
	if err != nil {
		return err
	}
	defer ovs.Close()

	return ensureOVSBridgeAndAttachWithClient(ctx, ovs, bridgeName, ifaceName, vlanID)
}

// ensureOVSBridgeAndAttachWithClient ensures an OVS bridge exists and attaches ifaceName as a port,
// as an access port for vlanID when it is set.
// This version accepts a client parameter for testing.
func ensureOVSBridgeAndAttachWithClient(ctx context.Context, ovs libovsclient.Client, bridgeName, ifaceName string,
	vlanID *int32) error {
	// Cache for indexed operations
	if _, err := ovs.Monitor(ctx,
		ovs.NewMonitor(
			libovsclient.WithTable(&ovsmodel.OpenvSwitch{}),
			libovsclient.WithTable(&ovsmodel.Bridge{}),
			libovsclient.WithTable(&ovsmodel.Port{}),
			libovsclient.WithTable(&ovsmodel.Interface{}),
		),
	); err != nil {
		return fmt.Errorf("failed to setup monitor: %w", err)
	}

	bridgeUUID, err := EnsureBridge(ctx, ovs, bridgeName)
	if err != nil {
		return fmt.Errorf("failed to ensure OVS bridge %q exists: %w", bridgeName, err)
	}

	// Create the bridge management port (internal port) so the bridge appears as a Linux interface
	if err := ensureInternalPortForBridge(ctx, ovs, bridgeUUID, bridgeName); err != nil {
		return fmt.Errorf("failed to create internal port for bridge %q: %w", bridgeName, err)
	}

	if err := ensurePortAttachedToBridge(ctx, ovs, bridgeUUID, ifaceName, vlanID); err != nil {
		return fmt.Errorf("failed to attach veth %q to bridge %q: %w", ifaceName, bridgeName, err)
	}

	// Wait for OVS bridge interface to appear and bring it UP
	bridge, err := waitForOVSBridgeInterface(bridgeName)
	if err != nil {
		return err
	}
	if err := netlink.LinkSetUp(bridge); err != nil {
		return fmt.Errorf("failed to bring OVS bridge %q UP: %w", bridgeName, err)
	}
	slog.Debug("OVS bridge interface is UP", "name", bridgeName)

	return nil
}

func ensurePortAttachedToBridge(ctx context.Context, ovs libovsclient.Client, bridgeUUID, interfaceName string,
	vlanID *int32) error {
	iface := &ovsmodel.Interface{Name: interfaceName}
	interfaceUUID := ""
	err := ovs.Get(ctx, iface)
	if err != nil && !errors.Is(err, libovsclient.ErrNotFound) {
		return fmt.Errorf("failed to check if interface %q exists: %w", interfaceName, err)
	}
	if err == nil {
		interfaceUUID = iface.UUID
	}

	port := &ovsmodel.Port{Name: interfaceName}
	portUUID := ""
	err = ovs.Get(ctx, port)
	if err != nil && !errors.Is(err, libovsclient.ErrNotFound) {
		return fmt.Errorf("failed to check if port %q exists: %w", interfaceName, err)
	}
	if err == nil {
		portUUID = port.UUID
	}

	bridge := &ovsmodel.Bridge{UUID: bridgeUUID}
	if err := ovs.Get(ctx, bridge); err != nil {
		return fmt.Errorf("failed to get bridge: %w", err)
	}

	if portUUID != "" {
		if !slices.Contains(bridge.Ports, portUUID) {
			// A Port row that no bridge references is garbage-collected, so this one is on another
			// bridge. Inserting it here too would put one port in two bridges.
			return fmt.Errorf("port %q is attached to bridge %q, not to %q",
				interfaceName, bridgeHoldingPort(ctx, ovs, portUUID), bridge.Name)
		}
		if err := ensurePortVLAN(ctx, ovs, interfaceName, vlanID); err != nil {
			return fmt.Errorf("failed to set VLAN on port %q: %w", interfaceName, err)
		}
		slog.Debug("port already attached to bridge", "port", interfaceName, "bridge", bridge.Name)
		return nil
	}

	var operations []ovsdb.Operation

	interfaceNamedUUID := interfaceUUID
	if interfaceUUID == "" {
		interfaceOp, err := ovs.Create(
			&ovsmodel.Interface{
				UUID: "new_interface",
				Name: interfaceName,
				Type: "system", // system type for regular interfaces
			},
		)
		if err != nil {
			return fmt.Errorf("failed to create interface operation: %w", err)
		}
		operations = append(operations, interfaceOp...)
		interfaceNamedUUID = "new_interface"
	}

	portNamedUUID := portUUID
	if portUUID == "" {
		newPort := &ovsmodel.Port{
			UUID:       "new_port",
			Name:       interfaceName,
			Interfaces: []string{interfaceNamedUUID},
		}
		// The VLAN goes in with the port, so that it never forwards untagged.
		reconcilePortVLAN(newPort, vlanID)
		portOp, err := ovs.Create(newPort)
		if err != nil {
			return fmt.Errorf("failed to create port operation: %w", err)
		}
		operations = append(operations, portOp...)
		portNamedUUID = "new_port"
	}

	mutateOp, err := ovs.Where(bridge).Mutate(bridge, model.Mutation{
		Field:   &bridge.Ports,
		Mutator: ovsdb.MutateOperationInsert,
		Value:   []string{portNamedUUID},
	})
	if err != nil {
		return fmt.Errorf("failed to create bridge mutate operation: %w", err)
	}
	operations = append(operations, mutateOp...)

	slog.Info("executing OVS transaction to attach port to bridge",
		"interface", interfaceName,
		"bridge", bridge.Name,
		"operations_count", len(operations))

	reply, err := ovs.Transact(ctx, operations...)
	if err != nil {
		return fmt.Errorf("OVS transaction failed when attaching interface %s to bridge %s: %w", interfaceName, bridge.Name, err)
	}

	if _, err := ovsdb.CheckOperationResults(reply, operations); err != nil {
		return fmt.Errorf("OVS operation failed when attaching interface %s to bridge %s (this usually means the interface doesn't exist on the system or OVS can't access it): %w", interfaceName, bridge.Name, err)
	}

	slog.Info("successfully added interface to bridge", "name", interfaceName, "bridge", bridge.Name)

	return nil
}

// ensurePortVLAN makes an existing port an access port for vlanID, or, when
// vlanID is nil, releases what the controller set earlier (see reconcilePortVLAN).
// A concurrent change to the port aborts the write, and it is retried against
// the port as it is then.
func ensurePortVLAN(ctx context.Context, ovs libovsclient.Client, portName string, vlanID *int32) error {
	for attempt := 1; ; attempt++ {
		port := &ovsmodel.Port{Name: portName}
		if err := ovs.Get(ctx, port); err != nil {
			return fmt.Errorf("failed to get port %q: %w", portName, err)
		}
		err := applyPortVLAN(ctx, ovs, port, vlanID)
		if !errors.Is(err, errPortChanged) || attempt == portVLANAttempts {
			return err
		}
		slog.Info("port changed while setting its VLAN, retrying", "port", portName, "attempt", attempt)
		// Give the monitor time to deliver the change before reading the port again.
		// 100 ms is a heuristic, not a guarantee: if the cache is still stale, the
		// next attempt aborts on the wait again, and the last one reports it.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// applyPortVLAN writes the VLAN state reconcilePortVLAN computes for observed,
// in one transaction guarded by a wait on the columns it was computed from:
// if another client changed the port's tag, vlan_mode or external_ids since
// observed was read, the transaction aborts with errPortChanged. The
// controller's keys are changed with key-scoped mutations, so other
// external_ids entries are never rewritten.
func applyPortVLAN(ctx context.Context, ovs libovsclient.Client, observed *ovsmodel.Port, vlanID *int32) error {
	desired := observed.DeepCopy()
	reconcilePortVLAN(desired, vlanID)
	scalarFields := changedScalarFields(observed, desired)
	deleteKeys, insertIDs := controllerKeyChanges(observed.ExternalIDs, desired.ExternalIDs)
	if len(scalarFields) == 0 && len(deleteKeys) == 0 && len(insertIDs) == 0 {
		return nil
	}

	noWait := 0
	ops, err := ovs.Where(observed).Wait(ovsdb.WaitConditionEqual, &noWait, observed,
		&observed.Tag, &observed.VLANMode, &observed.ExternalIDs)
	if err != nil {
		return fmt.Errorf("failed to create port wait operation: %w", err)
	}
	if len(scalarFields) > 0 {
		updateOps, err := ovs.Where(desired).Update(desired, scalarFields...)
		if err != nil {
			return fmt.Errorf("failed to create port update operation: %w", err)
		}
		ops = append(ops, updateOps...)
	}
	var mutations []model.Mutation
	if len(deleteKeys) > 0 {
		mutations = append(mutations, model.Mutation{
			Field: &desired.ExternalIDs, Mutator: ovsdb.MutateOperationDelete, Value: deleteKeys,
		})
	}
	if len(insertIDs) > 0 {
		mutations = append(mutations, model.Mutation{
			Field: &desired.ExternalIDs, Mutator: ovsdb.MutateOperationInsert, Value: insertIDs,
		})
	}
	if len(mutations) > 0 {
		mutateOps, err := ovs.Where(desired).Mutate(desired, mutations...)
		if err != nil {
			return fmt.Errorf("failed to create port mutate operation: %w", err)
		}
		ops = append(ops, mutateOps...)
	}

	reply, err := ovs.Transact(ctx, ops...)
	if err != nil {
		return fmt.Errorf("OVS transaction failed: %w", err)
	}
	if waitTimedOut(reply) {
		return fmt.Errorf("port %q: %w", observed.Name, errPortChanged)
	}
	if _, err := ovsdb.CheckOperationResults(reply, ops); err != nil {
		return fmt.Errorf("OVS operation failed: %w", err)
	}

	slog.Info("reconciled VLAN on OVS port", "port", observed.Name, "vlanID", vlanID,
		"tag", desired.Tag, "vlanMode", desired.VLANMode)
	return nil
}

// reconcilePortVLAN sets the port's VLAN and owner key for vlanID, and makes it
// an access port: a vlan_mode other than access is replaced, and the previous
// mode is recorded under portVLANModeKey. With vlanID nil it only ever undoes
// what the controller did: the tag is cleared when the owner key is present and
// the tag still equals it, and a recorded mode is restored while the port is
// still in access mode. A tag or mode without the controller's keys, or one
// somebody changed after the controller set it, is left alone, and in the
// latter case the stale keys are dropped.
func reconcilePortVLAN(port *ovsmodel.Port, vlanID *int32) {
	if vlanID != nil {
		setPortVLAN(port, *vlanID)
		return
	}

	owner, owned := port.ExternalIDs[portVLANOwnerKey]
	if !owned {
		return
	}
	delete(port.ExternalIDs, portVLANOwnerKey)
	previousMode, modeChanged := port.ExternalIDs[portVLANModeKey]
	delete(port.ExternalIDs, portVLANModeKey)
	if port.Tag == nil || strconv.Itoa(*port.Tag) != owner {
		return
	}
	port.Tag = nil
	if modeChanged && isAccessMode(port.VLANMode) {
		restored := previousMode
		port.VLANMode = &restored
	}
}

func setPortVLAN(port *ovsmodel.Port, vlanID int32) {
	port.Tag = new(int(vlanID))
	if port.ExternalIDs == nil {
		port.ExternalIDs = map[string]string{}
	}
	port.ExternalIDs[portVLANOwnerKey] = strconv.Itoa(int(vlanID))
	// An empty vlan_mode with a tag is access already; any explicit other mode is not.
	if port.VLANMode == nil || isAccessMode(port.VLANMode) {
		return
	}
	if _, recorded := port.ExternalIDs[portVLANModeKey]; !recorded {
		port.ExternalIDs[portVLANModeKey] = *port.VLANMode
	}
	port.VLANMode = new(ovsmodel.PortVLANModeAccess)
}

// waitTimedOut tells whether the transaction was aborted by its leading wait:
// with a timeout of 0, OVSDB answers "timed out" when the row no longer matches.
// Any other error on the wait (a malformed one, say) is not a concurrent change.
func waitTimedOut(reply []ovsdb.OperationResult) bool {
	return len(reply) > 0 && reply[0].Error == "timed out"
}

func isAccessMode(mode *ovsmodel.PortVLANMode) bool {
	return mode != nil && *mode == ovsmodel.PortVLANModeAccess
}

// changedScalarFields lists the tag and vlan_mode fields of desired that differ from observed.
func changedScalarFields(observed, desired *ovsmodel.Port) []any {
	var fields []any
	if !equalTag(observed.Tag, desired.Tag) {
		fields = append(fields, &desired.Tag)
	}
	if !equalMode(observed.VLANMode, desired.VLANMode) {
		fields = append(fields, &desired.VLANMode)
	}
	return fields
}

// controllerKeyChanges returns the controller's external_ids keys to delete and
// the entries to insert to turn observed into desired. A changed value is a
// delete followed by an insert, since an OVSDB map insert never overwrites.
func controllerKeyChanges(observed, desired map[string]string) ([]string, map[string]string) {
	var deleteKeys []string
	insertIDs := map[string]string{}
	for _, key := range []string{portVLANOwnerKey, portVLANModeKey} {
		oldValue, had := observed[key]
		newValue, has := desired[key]
		if had && (!has || oldValue != newValue) {
			deleteKeys = append(deleteKeys, key)
		}
		if has && (!had || oldValue != newValue) {
			insertIDs[key] = newValue
		}
	}
	if len(insertIDs) == 0 {
		insertIDs = nil
	}
	return deleteKeys, insertIDs
}

func equalMode(a, b *ovsmodel.PortVLANMode) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func equalTag(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// bridgeHoldingPort names the bridge that references portUUID, for error messages.
func bridgeHoldingPort(ctx context.Context, ovs libovsclient.Client, portUUID string) string {
	var bridges []ovsmodel.Bridge
	err := ovs.WhereCache(func(b *ovsmodel.Bridge) bool { return slices.Contains(b.Ports, portUUID) }).
		List(ctx, &bridges)
	if err != nil || len(bridges) == 0 {
		return "<unknown>"
	}
	return bridges[0].Name
}

// ensureInternalPortForBridge creates an OVS internal port for the bridge.
// This creates the Linux network interface that can be used as a master for macvlan/ipvlan.
func ensureInternalPortForBridge(ctx context.Context, ovs libovsclient.Client, bridgeUUID, bridgeName string) error {
	// Check if internal port already exists
	iface := &ovsmodel.Interface{Name: bridgeName}
	err := ovs.Get(ctx, iface)
	if err == nil {
		slog.Debug("internal port already exists for bridge", "name", bridgeName, "UUID", iface.UUID)
		return nil
	}
	if !errors.Is(err, libovsclient.ErrNotFound) {
		return fmt.Errorf("failed to check if internal interface %q exists: %w", bridgeName, err)
	}
	slog.Debug("bridge management port does not exist; must create it", "name", bridgeName)

	port := &ovsmodel.Port{Name: bridgeName}
	err = ovs.Get(ctx, port)
	if err == nil {
		slog.Debug("internal port already exists for bridge", "name", bridgeName, "UUID", port.UUID)
		return nil
	}
	if !errors.Is(err, libovsclient.ErrNotFound) {
		return fmt.Errorf("failed to check if internal port %q exists: %w", bridgeName, err)
	}
	slog.Debug("bridge management port does not exist; must create it", "name", bridgeName)

	// Create internal interface and port for the bridge
	var operations []ovsdb.Operation
	interfaceNamedUUID := "new_internal_interface"
	portNamedUUID := "new_internal_port"

	// Create internal interface (this creates the Linux interface)
	interfaceOp, err := ovs.Create(
		&ovsmodel.Interface{
			UUID: interfaceNamedUUID,
			Name: bridgeName,
			Type: "internal", // internal type creates a Linux interface
		},
	)
	if err != nil {
		return fmt.Errorf("failed to create internal interface operation: %w", err)
	}
	operations = append(operations, interfaceOp...)

	// Create port for the internal interface
	portOp, err := ovs.Create(
		&ovsmodel.Port{
			UUID:       portNamedUUID,
			Name:       bridgeName,
			Interfaces: []string{interfaceNamedUUID},
		},
	)
	if err != nil {
		return fmt.Errorf("failed to create internal port operation: %w", err)
	}
	operations = append(operations, portOp...)

	// Attach the internal port to the bridge
	bridge := &ovsmodel.Bridge{UUID: bridgeUUID}
	if err := ovs.Get(ctx, bridge); err != nil {
		return fmt.Errorf("failed to get bridge: %w", err)
	}

	mutateOp, err := ovs.Where(bridge).Mutate(bridge, model.Mutation{
		Field:   &bridge.Ports,
		Mutator: ovsdb.MutateOperationInsert,
		Value:   []string{portNamedUUID},
	})
	if err != nil {
		return fmt.Errorf("failed to create bridge mutate operation: %w", err)
	}
	operations = append(operations, mutateOp...)

	slog.Info("creating OVS internal port for bridge",
		"bridge", bridgeName,
		"operations_count", len(operations))

	reply, err := ovs.Transact(ctx, operations...)
	if err != nil {
		return fmt.Errorf("OVS transaction failed when creating internal port for bridge %s: %w", bridgeName, err)
	}

	if _, err = ovsdb.CheckOperationResults(reply, operations); err != nil {
		return fmt.Errorf("OVS operation failed when creating internal port for bridge %s: %w", bridgeName, err)
	}

	slog.Info("successfully created internal port for bridge", "name", bridgeName)
	return nil
}

// waitForOVSBridgeInterface waits for an OVS bridge interface to appear
func waitForOVSBridgeInterface(name string) (netlink.Link, error) {
	if link, err := netlink.LinkByName(name); err == nil {
		return link, nil
	}

	ch := make(chan netlink.LinkUpdate)
	done := make(chan struct{})
	defer close(done)

	if err := netlink.LinkSubscribe(ch, done); err != nil {
		return nil, fmt.Errorf("failed to subscribe to link updates: %w", err)
	}

	timeout := time.After(5 * time.Second)
	for {
		select {
		case update := <-ch:
			if update.Link.Attrs().Name == name {
				return update.Link, nil
			}
		case <-timeout:
			return nil, fmt.Errorf("timeout waiting for OVS bridge interface %q to appear", name)
		}
	}
}
