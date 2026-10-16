// SPDX-License-Identifier:Apache-2.0

package hostnetwork

import "github.com/vishvananda/netlink"

// NetlinkOption is a function applied to a Link after creation but before link-up.
type NetlinkOption func(link netlink.Link) error

// linkList is a variable so that tests can count the dumps of every link of a
// namespace that a setup takes.
var linkList = netlink.LinkList
