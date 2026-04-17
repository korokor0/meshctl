package mesh

import (
	"encoding/binary"
	"fmt"
	"net/netip"

	"github.com/honoka/meshctl/internal/config"
)

// AssignAddresses assigns deterministic tunnel addresses to all links
// based on each node's node_id.
//
// ALL links get V4LL point-to-point addresses (base + node_id) because
// OSPFv3 IPv4 AF needs an IPv4 address on each interface for nexthop
// resolution. Fe80 addresses are separately derived from node_id at
// generation time (fe80::127:<node_id>).
//
// Example with base 169.254.0.0, node_id 2 and 4:
//
//	AddrA = 169.254.0.2/32, AddrB = 169.254.0.4/32
func AssignAddresses(links []Link, cfg *config.Config) error {
	prefix, err := netip.ParsePrefix(cfg.Global.LinkLocalV4Range)
	if err != nil {
		return fmt.Errorf("invalid linklocal_v4_range: %w", err)
	}

	base := prefix.Addr()

	for i := range links {
		nodeA := cfg.NodeByName(links[i].NodeA)
		nodeB := cfg.NodeByName(links[i].NodeB)
		if nodeA == nil || nodeB == nil {
			return fmt.Errorf("link %s-%s: node not found", links[i].NodeA, links[i].NodeB)
		}

		addrA, err := nodeAddr(base, nodeA.NodeID)
		if err != nil {
			return fmt.Errorf("link %s-%s: %w", links[i].NodeA, links[i].NodeB, err)
		}
		addrB, err := nodeAddr(base, nodeB.NodeID)
		if err != nil {
			return fmt.Errorf("link %s-%s: %w", links[i].NodeA, links[i].NodeB, err)
		}

		links[i].AddrA = addrA
		links[i].AddrB = addrB
	}
	return nil
}

// nodeAddr computes the V4LL address for a node: base + nodeID, as /32.
func nodeAddr(base netip.Addr, nodeID int) (netip.Prefix, error) {
	b := base.As4()
	baseVal := binary.BigEndian.Uint32(b[:])
	val := baseVal + uint32(nodeID)

	var a4 [4]byte
	binary.BigEndian.PutUint32(a4[:], val)
	addr := netip.AddrFrom4(a4)
	return netip.PrefixFrom(addr, 32), nil
}

// Fe80ForNode returns the fe80 link-local address for a node: fe80::127:<nodeID>.
func Fe80ForNode(nodeID int) string {
	return fmt.Sprintf("fe80::127:%x", nodeID)
}
