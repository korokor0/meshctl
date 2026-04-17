// Package mesh computes the overlay mesh topology: peer pairs, link modes,
// and deterministic tunnel addressing.
package mesh

import (
	"fmt"
	"net/netip"
	"sort"

	"github.com/honoka/meshctl/internal/config"
)

// LinkMode determines the addressing scheme used for a tunnel link.
type LinkMode int

const (
	// LinkModeFe80 uses IPv6 link-local addresses with OSPFv3 AF.
	// Only valid when both endpoints are Linux.
	LinkModeFe80 LinkMode = iota

	// LinkModeV4LL uses 169.254.x.x/31 with OSPFv2.
	// Required when any endpoint is RouterOS or static.
	LinkModeV4LL
)

// Link represents a single WireGuard tunnel between two nodes.
type Link struct {
	NodeA string       // sorted lexicographically: A < B
	NodeB string
	Mode  LinkMode
	AddrA netip.Prefix // set only for LinkModeV4LL
	AddrB netip.Prefix // set only for LinkModeV4LL
}

// ComputeLinks enumerates all peer links based on the link policy and
// per-node peers_with constraints. Returns an error if any link has
// both endpoints without a reachable address (no public IP on either side).
func ComputeLinks(cfg *config.Config) ([]Link, error) {
	pairs := enumeratePairs(cfg)
	links := make([]Link, 0, len(pairs))
	for _, p := range pairs {
		a := cfg.NodeByName(p[0])
		b := cfg.NodeByName(p[1])
		if !a.HasEndpoint() && !b.HasEndpoint() {
			return nil, fmt.Errorf("link %s <-> %s: both nodes have no endpoint; at least one side must have a public IP or domain", p[0], p[1])
		}
		mode := selectLinkMode(a, b)
		links = append(links, Link{
			NodeA: p[0],
			NodeB: p[1],
			Mode:  mode,
		})
	}
	return links, nil
}

// enumeratePairs returns sorted, deduplicated [nameA, nameB] pairs
// where nameA < nameB lexicographically.
func enumeratePairs(cfg *config.Config) [][2]string {
	// Build the set of constrained nodes (those with peers_with).
	constrained := make(map[string]map[string]bool)
	for _, n := range cfg.Nodes {
		if len(n.PeersWith) > 0 {
			set := make(map[string]bool)
			for _, p := range n.PeersWith {
				set[p] = true
			}
			constrained[n.Name] = set
		}
	}

	seen := make(map[[2]string]bool)
	var pairs [][2]string

	if cfg.LinkPolicy.Mode == "full" {
		// Full mesh: every pair, respecting peers_with constraints.
		for i := 0; i < len(cfg.Nodes); i++ {
			for j := i + 1; j < len(cfg.Nodes); j++ {
				a, b := sortPair(cfg.Nodes[i].Name, cfg.Nodes[j].Name)
				pair := [2]string{a, b}
				if seen[pair] {
					continue
				}
				// If either node has peers_with, the other must be listed.
				if !peerAllowed(a, b, constrained) {
					continue
				}
				seen[pair] = true
				pairs = append(pairs, pair)
			}
		}
	}

	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i][0] != pairs[j][0] {
			return pairs[i][0] < pairs[j][0]
		}
		return pairs[i][1] < pairs[j][1]
	})
	return pairs
}

// peerAllowed returns true if the pair is permitted given peers_with constraints.
// If neither node has a constraint, the pair is allowed.
// If a node has peers_with, its partner must appear in the list.
func peerAllowed(a, b string, constrained map[string]map[string]bool) bool {
	ca, aHas := constrained[a]
	cb, bHas := constrained[b]
	if aHas && !ca[b] {
		return false
	}
	if bHas && !cb[a] {
		return false
	}
	return true
}

// selectLinkMode returns Fe80 if both nodes are Linux, V4LL otherwise.
func selectLinkMode(a, b *config.Node) LinkMode {
	if a.Type == config.NodeTypeLinux && b.Type == config.NodeTypeLinux {
		return LinkModeFe80
	}
	return LinkModeV4LL
}

// sortPair returns two strings in lexicographic order.
func sortPair(a, b string) (string, string) {
	if a <= b {
		return a, b
	}
	return b, a
}

// LinksForNode returns all links where the given node participates.
func LinksForNode(links []Link, nodeName string) []Link {
	var result []Link
	for _, l := range links {
		if l.NodeA == nodeName || l.NodeB == nodeName {
			result = append(result, l)
		}
	}
	return result
}

// PeerName returns the name of the other node in a link.
func (l *Link) PeerName(self string) string {
	if l.NodeA == self {
		return l.NodeB
	}
	return l.NodeA
}

// SelfAddr returns the address for the given node in a V4LL link.
func (l *Link) SelfAddr(self string) netip.Prefix {
	if l.NodeA == self {
		return l.AddrA
	}
	return l.AddrB
}

// PeerAddr returns the address of the peer in a V4LL link.
func (l *Link) PeerAddr(self string) netip.Prefix {
	if l.NodeA == self {
		return l.AddrB
	}
	return l.AddrA
}
