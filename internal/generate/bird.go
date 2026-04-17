package generate

import (
	"bytes"
	"embed"
	"fmt"
	"strings"
	"text/template"

	"github.com/honoka/meshctl/internal/config"
	"github.com/honoka/meshctl/internal/mesh"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

// BIRDGenerator produces BIRD configuration for Linux fat nodes.
type BIRDGenerator struct {
	cfg       *config.Config
	templates *template.Template
}

// NewBIRDGenerator creates a BIRD config generator.
func NewBIRDGenerator(cfg *config.Config) (*BIRDGenerator, error) {
	prefix := cfg.Global.WGIfacePrefix
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"ifaceName": func(peerName string) string {
			return WGInterfaceName(prefix, peerName)
		},
		"derefUint32": func(p *uint32) uint32 { return *p },
		"peerCost": func(costs map[string]uint32, peer string) uint32 {
			if c, ok := costs[peer]; ok {
				return c
			}
			return defaultDynamicCost
		},
	}).ParseFS(templateFS, "templates/*.tmpl")
	if err != nil {
		return nil, fmt.Errorf("parsing templates: %w", err)
	}
	return &BIRDGenerator{cfg: cfg, templates: tmpl}, nil
}

// UnderlayRoute represents a static route for reaching a peer's endpoint
// on the underlay network, with krt_prefsrc to pin the source address.
// The "via" gateway is left empty here — the agent detects it at runtime.
type UnderlayRoute struct {
	PeerName string // for comment
	Dest     string // peer endpoint IP (/128 or /32)
	Prefsrc  string // krt_prefsrc value
}

// birdData is the template context for BIRD config generation.
type birdData struct {
	Node            *config.Node
	Global          *config.Global
	Peers           []WGPeerConfig
	Links           []mesh.Link
	Fe80Links       []mesh.Link
	V4LLLinks       []mesh.Link
	PeerCosts       map[string]uint32 // peer name → initial OSPF cost
	UnderlayRoutes4 []UnderlayRoute   // IPv4 underlay static routes
	UnderlayRoutes6 []UnderlayRoute   // IPv6 underlay static routes
}

// GenerateWireguard produces a wireguard.json for a Linux fat node.
func (g *BIRDGenerator) GenerateWireguard(node *config.Node, peers []WGPeerConfig) ([]byte, error) {
	// Build underlay route info for the agent.
	// Dual-stack nodes may have both IPv4 and IPv6 endpoints.
	var ur4, ur6 []UnderlayRoute
	prefsrc4 := node.EffectivePrefsrc4()
	prefsrc6 := node.EffectivePrefsrc6()
	if node.Underlay != nil {
		for _, p := range peers {
			peerNode := g.cfg.NodeByName(p.Name)
			if peerNode == nil {
				continue
			}
			if v4 := peerNode.EndpointIPv4(); v4.IsValid() && prefsrc4 != "" {
				ur4 = append(ur4, UnderlayRoute{
					PeerName: p.Name,
					Dest:     v4.String() + "/32",
					Prefsrc:  prefsrc4,
				})
			}
			if v6 := peerNode.EndpointIPv6(); v6.IsValid() && prefsrc6 != "" {
				ur6 = append(ur6, UnderlayRoute{
					PeerName: p.Name,
					Dest:     v6.String() + "/128",
					Prefsrc:  prefsrc6,
				})
			}
		}
	}

	// Build cost band config for the agent.
	type CostBandJSON struct {
		Up   int64  `json:"up_ms"`
		Down int64  `json:"down_ms"`
		Cost uint32 `json:"cost"`
		Hold int    `json:"hold"`
	}
	var costBands []CostBandJSON
	for _, b := range g.cfg.Global.CostBands {
		costBands = append(costBands, CostBandJSON{
			Up:   b.Up.Milliseconds(),
			Down: b.Down.Milliseconds(),
			Cost: b.Cost,
			Hold: b.Hold,
		})
	}

	var buf bytes.Buffer
	err := g.templates.ExecuteTemplate(&buf, "linux_wireguard.json.tmpl", struct {
		Node            *config.Node
		Peers           []WGPeerConfig
		PSKRequired     bool
		IGPTable4       string
		IGPTable6       string
		UnderlayRoutes4 []UnderlayRoute
		UnderlayRoutes6 []UnderlayRoute
		CostBands       []CostBandJSON
		PenaltyCost     uint32
		EWMAAlpha       float64
		FailThreshold   int
	}{
		Node:            node,
		Peers:           peers,
		PSKRequired:     g.cfg.Global.PSKEnabled,
		IGPTable4:       g.cfg.Global.IGPTable4,
		IGPTable6:       g.cfg.Global.IGPTable6,
		UnderlayRoutes4: ur4,
		UnderlayRoutes6: ur6,
		CostBands:       costBands,
		PenaltyCost:     g.cfg.Global.PenaltyCost,
		EWMAAlpha:       g.cfg.Global.EWMAAlpha,
		FailThreshold:   g.cfg.Global.ProbeFailThreshold,
	})
	if err != nil {
		return nil, fmt.Errorf("generating wireguard config: %w", err)
	}
	return buf.Bytes(), nil
}

// GenerateOSPF produces the bird-meshctl.conf include for a Linux fat node.
func (g *BIRDGenerator) GenerateOSPF(node *config.Node, links []mesh.Link) ([]byte, error) {
	nodeLinks := mesh.LinksForNode(links, node.Name)
	// Build peers to get cost info.
	peers := BuildWGPeers(g.cfg, node.Name, links)
	data := g.buildData(node, peers, nodeLinks)

	var buf bytes.Buffer
	if err := g.templates.ExecuteTemplate(&buf, "bird_meshctl.conf.tmpl", data); err != nil {
		return nil, fmt.Errorf("generating BIRD config: %w", err)
	}
	return buf.Bytes(), nil
}

// GenerateFull produces both wireguard.json and bird-meshctl.conf.
func (g *BIRDGenerator) GenerateFull(node *config.Node, peers []WGPeerConfig, links []mesh.Link) ([]byte, error) {
	// For BIRD, "full" is the OSPF config. WG config is separate.
	return g.GenerateOSPF(node, links)
}

// defaultDynamicCost is the initial OSPF cost for dynamically probed links.
const defaultDynamicCost = 100

func (g *BIRDGenerator) buildData(node *config.Node, peers []WGPeerConfig, links []mesh.Link) birdData {
	var fe80, v4ll []mesh.Link
	for _, l := range links {
		switch l.Mode {
		case mesh.LinkModeFe80:
			fe80 = append(fe80, l)
		case mesh.LinkModeV4LL:
			v4ll = append(v4ll, l)
		}
	}

	// Build per-peer initial cost map.
	peerCosts := make(map[string]uint32)
	for _, p := range peers {
		if p.CostMode == config.CostModeStatic && p.StaticCost != nil {
			peerCosts[p.Name] = *p.StaticCost
		} else if p.StaticCost != nil {
			// Probe mode with static_cost: use static_cost as initial value.
			// The agent will dynamically adjust it.
			peerCosts[p.Name] = *p.StaticCost
		} else {
			peerCosts[p.Name] = defaultDynamicCost
		}
	}

	// Build underlay static routes if the node has underlay config.
	var ur4, ur6 []UnderlayRoute
	bPrefsrc4 := node.EffectivePrefsrc4()
	bPrefsrc6 := node.EffectivePrefsrc6()
	if node.Underlay != nil {
		for _, p := range peers {
			peerNode := g.cfg.NodeByName(p.Name)
			if peerNode == nil {
				continue
			}
			if v4 := peerNode.EndpointIPv4(); v4.IsValid() && bPrefsrc4 != "" {
				ur4 = append(ur4, UnderlayRoute{
					PeerName: p.Name,
					Dest:     v4.String() + "/32",
					Prefsrc:  bPrefsrc4,
				})
			}
			if v6 := peerNode.EndpointIPv6(); v6.IsValid() && bPrefsrc6 != "" {
				ur6 = append(ur6, UnderlayRoute{
					PeerName: p.Name,
					Dest:     v6.String() + "/128",
					Prefsrc:  bPrefsrc6,
				})
			}
		}
	}

	return birdData{
		Node:            node,
		Global:          &g.cfg.Global,
		Peers:           peers,
		Links:           links,
		Fe80Links:       fe80,
		V4LLLinks:       v4ll,
		PeerCosts:       peerCosts,
		UnderlayRoutes4: ur4,
		UnderlayRoutes6: ur6,
	}
}

// WGInterfaceName returns the WireGuard interface name for a peer.
// Format: <prefix><peername> (lowercase, max 15 chars for Linux IFNAMSIZ).
// When the full name exceeds 15 chars, non-alphanumeric characters are
// stripped from the peer name first to preserve more meaningful content
// before truncating.
func WGInterfaceName(prefix, peerName string) string {
	name := prefix + strings.ToLower(peerName)
	if len(name) <= 15 {
		return name
	}
	// Strip non-alphanumeric characters from peer name to reclaim space.
	lower := strings.ToLower(peerName)
	var stripped []byte
	for i := 0; i < len(lower); i++ {
		c := lower[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			stripped = append(stripped, c)
		}
	}
	name = prefix + string(stripped)
	if len(name) > 15 {
		name = name[:15]
	}
	return name
}
