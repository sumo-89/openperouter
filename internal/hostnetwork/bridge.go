// SPDX-License-Identifier:Apache-2.0

package hostnetwork

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/vishvananda/netlink"
)

// setupBridge creates the bridge, looks up and binds the VRF when
// params.VRF is non-empty, applies any bridge options, and brings the link up.
func setupBridge(params VNIParams, opts ...NetlinkOption) (*netlink.Bridge, error) {
	name := BridgeName(params.VNI)
	bridge, err := createBridge(name)
	if err != nil {
		return nil, err
	}

	if params.VRF != "" {
		vrf, err := lookupVRF(params.VRF)
		if err != nil {
			return nil, fmt.Errorf("could not find vrf %s for bridge %s: %w", params.VRF, name, err)
		}
		if err := ensureBridgeMaster(bridge, vrf.Index); err != nil {
			return nil, err
		}
	}

	for _, opt := range opts {
		if err := opt(bridge); err != nil {
			return nil, fmt.Errorf("failed to apply bridge option for %s: %w", bridge.Name, err)
		}
	}

	err = linkSetUp(bridge)
	if err != nil {
		return nil, fmt.Errorf("could not set link up for bridge %s: %v", name, err)
	}
	return bridge, nil
}

func createBridge(name string) (*netlink.Bridge, error) {
	toCreate := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{
		Name: name,
	}}

	link, err := netlink.LinkByName(name)
	if errors.As(err, &netlink.LinkNotFoundError{}) {
		if err := netlink.LinkAdd(toCreate); err != nil {
			return nil, fmt.Errorf("could not create bridge %s: %w", name, err)
		}
		return toCreate, nil
	}
	if err != nil {
		return nil, fmt.Errorf("could find bridge by name %s: %w", name, err)
	}

	bridge, ok := link.(*netlink.Bridge)
	if ok {
		return bridge, nil
	}

	// link exists but it's not a bridge, delete and recreate
	err = netlink.LinkDel(link)
	if err != nil {
		return nil, fmt.Errorf("failed to delete link %v: %w", link, err)
	}
	if err := netlink.LinkAdd(toCreate); err != nil {
		return nil, fmt.Errorf("could not create bridge %s: %w", name, err)
	}

	return toCreate, nil
}

func ensureBridgeMaster(bridge *netlink.Bridge, masterIndex int) error {
	if bridge.MasterIndex == masterIndex {
		return nil
	}
	if err := netlink.LinkSetMasterByIndex(bridge, masterIndex); err != nil {
		return fmt.Errorf("could not bind bridge %s to VRF (index %d): %w", bridge.Name, masterIndex, err)
	}
	bridge.MasterIndex = masterIndex
	return nil
}

const (
	macSize = 6
)

var macHeader = []byte{0x00, 0xF3}

// BridgeFixedMAC returns the deterministic anycast MAC address for a given VNI.
// All nodes must use the same MAC for distributed L2 gateways.
func BridgeFixedMAC(vni int32) ([]byte, error) {
	macAddress := make([]byte, macSize)
	buf := new(bytes.Buffer)
	err := binary.Write(buf, binary.BigEndian, vni+1)
	if err != nil {
		return nil, err
	}
	copy(macAddress, macHeader)
	copy(macAddress[2:], buf.Bytes())
	return macAddress, nil
}

// GatewayMAC returns the MAC of the distributed gateway on an L2VNI bridge:
// the L2VNI's gatewayMAC when set, else the one derived from the VNI.
func GatewayMAC(params L2VNIParams) (net.HardwareAddr, error) {
	if params.GatewayMAC != nil {
		return params.GatewayMAC, nil
	}
	mac, err := BridgeFixedMAC(params.VNI)
	return net.HardwareAddr(mac), err
}

// ensureBridgeFixedMacAddress sets the given MAC on the bridge.
// It is idempotent: if the MAC is already correct, it skips the update to avoid
// unnecessary RTM_NEWLINK events that can cause FRR to flush neighbor entries.
func ensureBridgeFixedMacAddress(bridge netlink.Link, mac net.HardwareAddr) error {
	if bytes.Equal(bridge.Attrs().HardwareAddr, mac) {
		return nil
	}

	if err := netlink.LinkSetHardwareAddr(bridge, mac); err != nil {
		return fmt.Errorf("failed to set mac address to bridge %s %s: %w", bridge.Attrs().Name, mac, err)
	}
	return nil
}

const bridgePrefix = "br-pe-"

// BridgeName returns the PE bridge name for a given VNI.
func BridgeName(vni int32) string {
	return fmt.Sprintf("%s%d", bridgePrefix, vni)
}

func vniFromBridgeName(name string) (int32, error) {
	if !strings.HasPrefix(name, bridgePrefix) {
		return 0, NotRouterInterfaceError{Name: name}
	}

	vni := strings.TrimPrefix(name, bridgePrefix)
	res, err := strconv.ParseInt(vni, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("failed to get vni for bridge %s", name)
	}
	return int32(res), nil
}
