package generate

import (
	"bytes"
	"fmt"

	"github.com/honoka/meshctl/internal/config"
	"github.com/honoka/meshctl/internal/mesh"
)

// StaticSnippetGenerator produces reference configuration snippets
// for static nodes that cannot be automatically configured.
type StaticSnippetGenerator struct {
	cfg *config.Config
}

// NewStaticSnippetGenerator creates a static node config generator.
func NewStaticSnippetGenerator(cfg *config.Config) *StaticSnippetGenerator {
	return &StaticSnippetGenerator{cfg: cfg}
}

// GenerateWireguard produces a WireGuard config snippet for a static node.
func (g *StaticSnippetGenerator) GenerateWireguard(node *config.Node, peers []WGPeerConfig) ([]byte, error) {
	var buf bytes.Buffer

	fmt.Fprintf(&buf, "# WireGuard reference configuration for %s (static node)\n", node.Name)
	fmt.Fprintf(&buf, "# Adapt this to your platform's WireGuard configuration format.\n\n")

	for _, p := range peers {
		iface := WGInterfaceName(g.cfg.Global.WGIfacePrefix,p.Name)
		fmt.Fprintf(&buf, "## Interface: %s (peer: %s)\n", iface, p.Name)
		fmt.Fprintf(&buf, "# PublicKey  = %s\n", p.PublicKey)
		if p.Endpoint != "" {
			fmt.Fprintf(&buf, "# Endpoint   = %s\n", p.Endpoint)
		} else {
			fmt.Fprintf(&buf, "# Endpoint   = (none — peer initiates connection)\n")
		}
		if p.InterfaceIP != "" {
			fmt.Fprintf(&buf, "# Address    = %s peer %s/32\n", p.InterfaceIP, p.PeerIP)
		}
		if p.Fe80IP != "" {
			fmt.Fprintf(&buf, "# Fe80       = %s/64\n", p.Fe80IP)
		}
		fmt.Fprintf(&buf, "# AllowedIPs = %v\n", p.AllowedIPs)
		if p.KeepAlive > 0 {
			fmt.Fprintf(&buf, "# PersistentKeepalive = %d\n", p.KeepAlive)
		}
		fmt.Fprintf(&buf, "\n")
	}

	return buf.Bytes(), nil
}

// GenerateOSPF produces a routing reference snippet for a static node.
func (g *StaticSnippetGenerator) GenerateOSPF(node *config.Node, links []mesh.Link) ([]byte, error) {
	nodeLinks := mesh.LinksForNode(links, node.Name)
	var buf bytes.Buffer

	fmt.Fprintf(&buf, "# Routing reference for %s (static node)\n", node.Name)
	fmt.Fprintf(&buf, "# Configure your routing daemon with the following interfaces:\n\n")
	fmt.Fprintf(&buf, "# Router ID: %s\n", node.Loopback)
	fmt.Fprintf(&buf, "# OSPF Area: %s\n\n", g.cfg.Global.OSPFArea)

	for _, l := range nodeLinks {
		peerName := l.PeerName(node.Name)
		iface := WGInterfaceName(g.cfg.Global.WGIfacePrefix,peerName)
		fmt.Fprintf(&buf, "# Interface %s → peer %s\n", iface, peerName)
		if l.Mode == mesh.LinkModeV4LL {
			selfAddr := l.SelfAddr(node.Name)
			peerAddr := l.PeerAddr(node.Name)
			fmt.Fprintf(&buf, "#   Local:  %s\n", selfAddr)
			fmt.Fprintf(&buf, "#   Remote: %s\n", peerAddr.Addr())
		} else {
			fmt.Fprintf(&buf, "#   Mode: IPv6 link-local (fe80::)\n")
		}
		fmt.Fprintf(&buf, "\n")
	}

	return buf.Bytes(), nil
}

// GenerateFull produces a combined reference snippet.
func (g *StaticSnippetGenerator) GenerateFull(node *config.Node, peers []WGPeerConfig, links []mesh.Link) ([]byte, error) {
	var buf bytes.Buffer

	wg, err := g.GenerateWireguard(node, peers)
	if err != nil {
		return nil, err
	}
	buf.Write(wg)

	ospf, err := g.GenerateOSPF(node, links)
	if err != nil {
		return nil, err
	}
	buf.Write(ospf)

	return buf.Bytes(), nil
}
