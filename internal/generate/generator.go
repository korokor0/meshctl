// Package generate produces platform-specific configuration files
// from the mesh topology and node inventory.
package generate

import (
	"fmt"
	"sort"

	"github.com/honoka/meshctl/internal/config"
	"github.com/honoka/meshctl/internal/cost"
	"github.com/honoka/meshctl/internal/mesh"
)

// WGPeerConfig holds the WireGuard peer parameters for a single tunnel.
type WGPeerConfig struct {
	Name         string // peer node name
	PublicKey    string
	Endpoint     string // host:port (uses peer's port for this specific interface)
	AllowedIPs   []string
	KeepAlive    int
	ListenPort   int    // local listen port for this peer's interface
	InterfaceIP  string // local V4LL address (e.g. "169.254.0.4"), always set
	PeerIP       string // remote V4LL address (e.g. "169.254.0.2"), always set
	Fe80IP       string // local fe80 address (e.g. "fe80::127:4"), always set
	PeerFe80IP   string // peer's fe80 address (e.g. "fe80::127:3"), for probing
	LinkMode     mesh.LinkMode
	PeerType         config.NodeType  // type of the peer node
	CostMode         config.CostMode  // how the fat node determines cost for this peer
	StaticCost       *uint32          // fixed cost (when cost_mode=static, or fallback)
	BandwidthPenalty uint32           // additive OSPF cost for low-bandwidth links
}

// ConfigGenerator produces platform-specific config files.
type ConfigGenerator interface {
	// GenerateWireguard produces WireGuard configuration for a node.
	GenerateWireguard(node *config.Node, peers []WGPeerConfig) ([]byte, error)

	// GenerateOSPF produces OSPF/routing configuration for a node.
	GenerateOSPF(node *config.Node, links []mesh.Link) ([]byte, error)

	// GenerateFull produces a complete config bundle for a node.
	GenerateFull(node *config.Node, peers []WGPeerConfig, links []mesh.Link) ([]byte, error)
}

// PeerPort returns the listen port that nodeName uses for its interface
// to peerName. If peerName has wg_peer_port set, that port is used
// (all peers of that node use the same designated port). Otherwise
// ports are auto-assigned from nodeName's base port, sorted alphabetically,
// skipping ports already claimed by fixed wg_peer_port assignments.
func PeerPort(cfg *config.Config, nodeName string, peerName string, links []mesh.Link) int {
	peerNode := cfg.NodeByName(peerName)
	if peerNode != nil && peerNode.WGPeerPort != 0 {
		return peerNode.WGPeerPort
	}

	// Auto-assign: collect all peers, sort, assign from base skipping fixed ports.
	node := cfg.NodeByName(nodeName)
	base := node.EffectiveWGPort(cfg.Global.WGListenPort)
	nodeLinks := mesh.LinksForNode(links, nodeName)

	// Collect fixed ports used on this node.
	fixedPorts := make(map[int]bool)
	var autoPeers []string
	for _, l := range nodeLinks {
		p := l.PeerName(nodeName)
		pn := cfg.NodeByName(p)
		if pn != nil && pn.WGPeerPort != 0 {
			fixedPorts[pn.WGPeerPort] = true
		} else {
			autoPeers = append(autoPeers, p)
		}
	}
	sort.Strings(autoPeers)

	// Assign sequential ports from base, skipping fixed.
	port := base
	for _, name := range autoPeers {
		for fixedPorts[port] {
			port++
		}
		if name == peerName {
			return port
		}
		port++
	}
	return base // shouldn't reach here
}

// CheckInterfaceNameCollisions detects interface name truncation collisions
// across all nodes. Returns an error listing any collisions found.
func CheckInterfaceNameCollisions(cfg *config.Config, links []mesh.Link) error {
	// Check per node: each peer's interface name must be unique.
	for _, node := range cfg.Nodes {
		nodeLinks := mesh.LinksForNode(links, node.Name)
		seen := make(map[string]string) // iface name → peer name
		for _, l := range nodeLinks {
			peer := l.PeerName(node.Name)
			iface := WGInterfaceName(cfg.Global.WGIfacePrefix, peer)
			if prev, ok := seen[iface]; ok {
				return fmt.Errorf("node %s: interface name collision: peers %q and %q both produce interface %q (name truncated to 15 chars)",
					node.Name, prev, peer, iface)
			}
			seen[iface] = peer
		}
	}
	return nil
}

// BuildWGPeers constructs WGPeerConfig entries for a given node from the
// computed links and config. Each peer interface gets a unique listen port
// starting from the node's base port, assigned in alphabetical peer order.
func BuildWGPeers(cfg *config.Config, nodeName string, links []mesh.Link) []WGPeerConfig {
	nodeLinks := mesh.LinksForNode(links, nodeName)
	peers := make([]WGPeerConfig, 0, len(nodeLinks))

	localNode := cfg.NodeByName(nodeName)

	for _, l := range nodeLinks {
		peerName := l.PeerName(nodeName)
		peerNode := cfg.NodeByName(peerName)
		if peerNode == nil {
			continue
		}

		// Local listen port for igp-<peer> on this node.
		localPort := PeerPort(cfg, nodeName, peerName, links)
		// Remote endpoint port: the port that peerNode's igp-<us> listens on.
		remotePort := PeerPort(cfg, peerName, nodeName, links)

		// Compute bandwidth penalty: link_bw = min(local, peer).
		localBw := localNode.EffectiveBandwidth(cfg.Global.BandwidthThreshold)
		peerBw := peerNode.EffectiveBandwidth(cfg.Global.BandwidthThreshold)
		linkBw := localBw
		if peerBw < linkBw {
			linkBw = peerBw
		}
		bwPenalty := cost.CalcBandwidthPenalty(linkBw, cfg.Global.BandwidthThreshold, cfg.Global.ReferenceBandwidth)

		peer := WGPeerConfig{
			Name:             peerName,
			PublicKey:         peerNode.PubKey,
			Endpoint:         peerNode.EndpointForPort(localNode, remotePort),
			AllowedIPs:       []string{"0.0.0.0/0", "::/0"},
			KeepAlive:        cfg.Global.WGKeepAlive,
			ListenPort:       localPort,
			Fe80IP:           mesh.Fe80ForNode(localNode.NodeID),
			PeerFe80IP:       mesh.Fe80ForNode(peerNode.NodeID),
			LinkMode:         l.Mode,
			PeerType:         peerNode.Type,
			CostMode:         peerNode.EffectiveCostMode(),
			StaticCost:       peerNode.StaticCost,
			BandwidthPenalty: bwPenalty,
		}

		// All links get V4LL addresses (OSPFv3 IPv4 AF needs IPv4 nexthop).
		peerAddr := l.PeerAddr(nodeName)
		selfAddr := l.SelfAddr(nodeName)
		if peerAddr.IsValid() {
			peer.PeerIP = peerAddr.Addr().String()
		}
		if selfAddr.IsValid() {
			peer.InterfaceIP = selfAddr.Addr().String()
		}

		peers = append(peers, peer)
	}
	return peers
}
