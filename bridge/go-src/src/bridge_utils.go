package main

import (
	"fmt"
	"strings"

	"github.com/docker/docker/libnetwork/iptables"
	"github.com/docker/docker/libnetwork/ns"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

const (
	bridgePrefix = "kt"
	bridgeLen    = 12
)

func getBridgeName(netID string) string {
	return bridgePrefix + "-" + netID[:bridgeLen]
}

func createBridge(netID , ipv4 string) (string, error) {
	bridgeName := getBridgeName(netID)

	exists, err := bridgeInterfaceExists(bridgeName)
	if err != nil {
		return "", err
	}

	if !exists {
		linkAttrs := netlink.NewLinkAttrs()
		linkAttrs.Name = bridgeName

		if err := netlink.LinkAdd(&netlink.Bridge{
			LinkAttrs: linkAttrs,
		}); err != nil {
			return "", err
		}
	}

	bridge, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return "", err
	}

	addr, err := netlink.ParseAddr(ipv4)
	if err != nil {
		return "", err
	}
	if err := netlink.AddrAdd(bridge, addr); err != nil {
		return "", err
	}

	var bridgeRule = []string{"-i", "eth0", "-o", bridgeName, "-j", "ACCEPT"}

	// Install rule in IPv4
	var iptablev4 = iptables.GetIptable(iptables.IPv4)
	if err := iptablev4.ProgramRule(iptables.Filter, "FORWARD", iptables.Append, bridgeRule); err != nil {
		return "", err
	}

	// Install rule in IPv6
	/*
	var iptablev6 = iptables.GetIptable(iptables.IPv6)
	if err := iptablev6.ProgramRule(iptables.Filter, "FORWARD", iptables.Append, bridgeRule); err != nil {
		return "", err
	}
	*/

	if err := patchBridge(bridge); err != nil {
		return "", err
	}

	return bridgeName, nil
}

func patchBridge(bridge netlink.Link) error {
	// Creates a new RTM_NEWLINK request
	// NLM_F_ACK is used to receive acks when operations are executed
	req := nl.NewNetlinkRequest(unix.RTM_NEWLINK, unix.NLM_F_ACK)

	// Search for the bridge interface by its index (and bring it UP too)
	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Change = unix.IFF_UP
	msg.Flags = unix.IFF_UP
	msg.Index = int32(bridge.Attrs().Index)
	req.AddData(msg)

	// Patch ageing_time and group_fwd_mask
	linkInfo := nl.NewRtAttr(unix.IFLA_LINKINFO, nil)
	linkInfo.AddRtAttr(nl.IFLA_INFO_KIND, nl.NonZeroTerminated(bridge.Type()))

	data := linkInfo.AddRtAttr(nl.IFLA_INFO_DATA, nil)
	data.AddRtAttr(nl.IFLA_BR_AGEING_TIME, nl.Uint32Attr(0))
	data.AddRtAttr(nl.IFLA_BR_GROUP_FWD_MASK, nl.Uint16Attr(0xfff8))

	req.AddData(linkInfo)

	// Execute the request. NETLINK_ROUTE is used to send link updates.
	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	if err != nil {
		return err
	}

	return nil
}

func deleteBridge(netID string) error {
	bridgeName := getBridgeName(netID)

	bridge, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return err
	}

	if err := netlink.LinkDel(bridge); err != nil {
		return err
	}

	/*
	var bridgeRule = []string{"-i", bridgeName, "-o", bridgeName, "-j", "ACCEPT"}

	// Delete rule in IPv4
	var iptablev4 = iptables.GetIptable(iptables.IPv4)
	if err := iptablev4.ProgramRule(iptables.Filter, "FORWARD", iptables.Delete, bridgeRule); err != nil {
		return err
	}

	// Delete rule in IPv6
	var iptablev6 = iptables.GetIptable(iptables.IPv6)
	if err := iptablev6.ProgramRule(iptables.Filter, "FORWARD", iptables.Delete, bridgeRule); err != nil {
		return err
	}
	*/

	return nil
}

func attachInterfaceToBridge(bridgeName string, interfaceName string) error {
	bridge, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return err
	}

	iface, err := netlink.LinkByName(interfaceName)
	if err != nil {
		return err
	}

	if err := netlink.LinkSetMaster(iface, bridge); err != nil {
		return err
	}
	if err := netlink.LinkSetUp(iface); err != nil {
		return err
	}

	return nil
}

func bridgeInterfaceExists(name string) (bool, error) {
	nlh := ns.NlHandle()
	link, err := nlh.LinkByName(name)

	if err != nil {
		if strings.Contains(err.Error(), "Link not found") {
			return false, nil
		}

		return false, fmt.Errorf("failed to check bridge interface existence: %v", err)
	}

	if link.Type() == "bridge" {
		return true, nil
	}

	return false, fmt.Errorf("existing interface %s is not a bridge", name)
}
