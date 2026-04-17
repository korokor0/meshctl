// Package config parses and validates the meshctl inventory YAML file.
package config

import (
	"fmt"
	"net/netip"
	"os"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

// NodeType represents the kind of mesh node.
type NodeType string

const (
	NodeTypeLinux    NodeType = "linux"
	NodeTypeRouterOS NodeType = "routeros"
	NodeTypeStatic   NodeType = "static"
)

// Config is the top-level inventory structure parsed from meshctl.yaml.
type Config struct {
	Global     Global     `yaml:"global"`
	Nodes      []Node     `yaml:"nodes"`
	LinkPolicy LinkPolicy `yaml:"link_policy"`
}

// Global contains mesh-wide defaults.
type Global struct {
	LinkLocalV4Range   string        `yaml:"linklocal_v4_range"`
	LinkLocalV4PrefLen int           `yaml:"linklocal_v4_prefix_len"`
	WGListenPort       int           `yaml:"wg_listen_port"`
	WGKeepAlive        int           `yaml:"wg_persistent_keepalive"`
	OSPFArea           string        `yaml:"ospf_area"`
	OSPFHello          int           `yaml:"ospf_hello"`
	OSPFDead           int           `yaml:"ospf_dead"`
	ProbePort          int           `yaml:"probe_port"`
	ProbeInterval      time.Duration `yaml:"probe_interval"`
	ProbeTimeout       time.Duration `yaml:"probe_timeout"`
	ProbeFailThreshold int           `yaml:"probe_failure_threshold"`
	CostBands          []CostBandDef `yaml:"cost_bands"`
	PenaltyCost        uint32        `yaml:"penalty_cost"`
	EWMAAlpha          float64       `yaml:"ewma_alpha"`
	OutputDir          string        `yaml:"output_dir"`

	// PSKEnabled signals that all fat-to-fat links in the mesh should use
	// a preshared key derived from a shared master secret. Enabling this
	// flag causes generated wireguard.json to set psk_required=true; the
	// agent on each node must then have psk_master_file configured.
	PSKEnabled bool `yaml:"psk_enabled"`

	// IGPTable4 and IGPTable6 are custom BIRD table names for OSPF routes.
	// OSPF protocols import/export to these tables instead of master4/master6,
	// so IGP routes are isolated. The operator pipes them to master as needed.
	// Defaults: "igptable4", "igptable6".
	IGPTable4 string `yaml:"igp_table4"`
	IGPTable6 string `yaml:"igp_table6"`

	// WGIfacePrefix is the prefix for WireGuard interface names.
	// The full name is <prefix><peer-name> (lowercase, truncated to 15 chars).
	// Default: "igp-".
	WGIfacePrefix string `yaml:"wg_iface_prefix"`

	// BandwidthThreshold is the bandwidth (Mbps) below which links receive
	// an additive OSPF cost penalty. Links at or above this value get no
	// penalty. Default: 300.
	BandwidthThreshold int `yaml:"bandwidth_threshold"`

	// ReferenceBandwidth is used in the Cisco-style auto-cost formula:
	// penalty = reference_bw/link_bw - reference_bw/threshold.
	// Default: 1000.
	ReferenceBandwidth int `yaml:"reference_bandwidth"`
}

// CostBandDef defines a single cost band in the inventory YAML.
type CostBandDef struct {
	Up   time.Duration `yaml:"up"`
	Down time.Duration `yaml:"down"`
	Cost uint32        `yaml:"cost"`
	Hold int           `yaml:"hold"`
}

// CostMode determines how a fat node calculates OSPF cost toward a peer.
type CostMode string

const (
	// CostModeProbe uses dynamic measurement (UDP one-way delay for fat peers,
	// ICMP rtt/2 for thin/static peers). This is the default.
	CostModeProbe CostMode = "probe"

	// CostModeStatic uses a fixed OSPF cost configured in the inventory.
	// The cost never changes regardless of measured latency.
	CostModeStatic CostMode = "static"
)

// Node describes a single mesh participant.
type Node struct {
	Name       string      `yaml:"name"`
	Type       NodeType    `yaml:"type"`
	Endpoint   EndpointDef `yaml:"endpoint"`
	Loopback   string      `yaml:"loopback"`
	Announce   []string    `yaml:"announce"`
	PubKey     string      `yaml:"pubkey"`
	PeersWith  []string    `yaml:"peers_with"`
	WGListPort int         `yaml:"wg_listen_port"`

	// NodeID is a unique numeric identifier for this node, used to derive
	// deterministic tunnel addresses:
	//   V4LL: linklocal_v4_range base + node_id (e.g. 169.254.0.2)
	//   Fe80: fe80::127:<node_id>/64
	// If unset, auto-assigned sequentially by node name order.
	NodeID int `yaml:"node_id"`

	// WGPeerPort is the designated port for this node. Every peer's
	// interface for this node listens on this port. If unset, the port
	// is auto-assigned from the peer's base port.
	WGPeerPort int `yaml:"wg_peer_port"`

	// Bandwidth is the node's uplink capacity in Mbps. Used to compute
	// bandwidth-aware OSPF cost penalty: link_bw = min(A.bandwidth, B.bandwidth).
	// If unset (0), defaults to global.bandwidth_threshold (no penalty).
	Bandwidth int `yaml:"bandwidth"`

	// CostMode controls how fat nodes determine OSPF cost for links to
	// this node. "probe" (default) uses ICMP rtt/2 for thin/static peers
	// or UDP one-way delay for fat peers. "static" uses StaticCost and
	// skips probing entirely.
	CostModeSetting CostMode `yaml:"cost_mode"`

	// StaticCost is the fixed OSPF cost used when cost_mode is "static".
	// Also used as fallback cost when cost_mode is "probe" but all probes
	// to this peer fail (instead of the global penalty cost).
	StaticCost *uint32 `yaml:"static_cost"`

	// Underlay configures automatic BIRD static routes for reaching peer
	// endpoints on the underlay network, with krt_prefsrc to pin the
	// source address. The agent auto-detects the default gateway at runtime.
	Underlay *UnderlayConfig `yaml:"underlay"`
}

// EndpointDef describes how to reach a node on the underlay network.
// At least one of IPv4, IPv6, or Domain must be set. The generator
// picks the best address per peer (prefers IPv6 when both sides support it).
// Domain is passed as-is to WireGuard for DNS resolution.
// Port comes from Node.WGListPort or Global.WGListenPort.
type EndpointDef struct {
	IPv4   string `yaml:"ipv4,omitempty"`   // e.g. "1.2.3.4"
	IPv6   string `yaml:"ipv6,omitempty"`   // e.g. "2001:db8::1"
	Domain string `yaml:"domain,omitempty"` // e.g. "us-west.example.com"
	DDNS   bool   `yaml:"ddns,omitempty"`   // reserved: agent re-resolves periodically
}

// UnderlayConfig specifies the preferred source addresses for underlay
// routes to peer endpoints.
//
// When underlay is present but prefsrc4/prefsrc6 are empty, they default
// to the node's own endpoint IP (from endpoint_addrs or parsed from
// endpoint). If the node doesn't have that address family, no underlay
// routes are generated for that family.
//
// Each prefsrc field accepts one of:
//   - Empty (default): uses the node's own endpoint IP for that family
//   - An explicit IP address: "2401:5a0:1000:93::a"
//   - An interface name: "ens3" — agent picks the primary IP from that interface
//   - "auto" — agent detects from the default route's source address
type UnderlayConfig struct {
	Prefsrc4 string `yaml:"prefsrc4"` // IPv4: IP, interface name, "auto", or empty (=endpoint)
	Prefsrc6 string `yaml:"prefsrc6"` // IPv6: IP, interface name, "auto", or empty (=endpoint)
}

// LinkPolicy controls how links between nodes are determined.
type LinkPolicy struct {
	Mode string `yaml:"mode"` // "full" for full-mesh
}

// LoopbackAddr returns the node's loopback as netip.Addr.
func (n *Node) LoopbackAddr() (netip.Addr, error) {
	addr, err := netip.ParseAddr(n.Loopback)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("node %s: invalid loopback %q: %w", n.Name, n.Loopback, err)
	}
	return addr, nil
}

// EffectiveBandwidth returns the node's bandwidth in Mbps. If unset, returns
// the given threshold so the node incurs no bandwidth penalty.
func (n *Node) EffectiveBandwidth(threshold int) int {
	if n.Bandwidth > 0 {
		return n.Bandwidth
	}
	return threshold
}

// EffectiveCostMode returns the cost mode for this node, defaulting to "probe".
func (n *Node) EffectiveCostMode() CostMode {
	if n.CostModeSetting == CostModeStatic {
		return CostModeStatic
	}
	return CostModeProbe
}

// EffectiveStaticCost returns the static cost if set, or 0 and false if unset.
func (n *Node) EffectiveStaticCost() (uint32, bool) {
	if n.StaticCost != nil {
		return *n.StaticCost, true
	}
	return 0, false
}

// EndpointIPv4 returns the node's IPv4 endpoint address, or invalid.
func (n *Node) EndpointIPv4() netip.Addr {
	if n.Endpoint.IPv4 != "" {
		addr, _ := netip.ParseAddr(n.Endpoint.IPv4)
		return addr
	}
	return netip.Addr{}
}

// EndpointIPv6 returns the node's IPv6 endpoint address, or invalid.
func (n *Node) EndpointIPv6() netip.Addr {
	if n.Endpoint.IPv6 != "" {
		addr, _ := netip.ParseAddr(n.Endpoint.IPv6)
		return addr
	}
	return netip.Addr{}
}

// EndpointFor returns the best WireGuard endpoint string ("host:port") for
// connecting to this node from localNode. Selection logic:
//   - If both support IPv6, prefer IPv6
//   - If only one IP protocol in common, use that
//   - If domain is set, use domain as fallback
func (n *Node) EndpointFor(localNode *Node, globalPort int) string {
	port := n.EffectiveWGPort(globalPort)

	remoteV6 := n.EndpointIPv6()
	remoteV4 := n.EndpointIPv4()

	// Prefer IPv6 when both sides support it.
	if localNode.EndpointIPv6().IsValid() && remoteV6.IsValid() {
		return fmt.Sprintf("[%s]:%d", remoteV6.String(), port)
	}
	if localNode.EndpointIPv4().IsValid() && remoteV4.IsValid() {
		return fmt.Sprintf("%s:%d", remoteV4.String(), port)
	}

	// Fallback: any available IP, or domain.
	if remoteV6.IsValid() {
		return fmt.Sprintf("[%s]:%d", remoteV6.String(), port)
	}
	if remoteV4.IsValid() {
		return fmt.Sprintf("%s:%d", remoteV4.String(), port)
	}
	if n.Endpoint.Domain != "" {
		return fmt.Sprintf("%s:%d", n.Endpoint.Domain, port)
	}
	return ""
}

// EndpointForPort returns the best WireGuard endpoint string using a
// specific port (for per-peer interface port assignment).
func (n *Node) EndpointForPort(localNode *Node, port int) string {
	remoteV6 := n.EndpointIPv6()
	remoteV4 := n.EndpointIPv4()

	if localNode.EndpointIPv6().IsValid() && remoteV6.IsValid() {
		return fmt.Sprintf("[%s]:%d", remoteV6.String(), port)
	}
	if localNode.EndpointIPv4().IsValid() && remoteV4.IsValid() {
		return fmt.Sprintf("%s:%d", remoteV4.String(), port)
	}
	if remoteV6.IsValid() {
		return fmt.Sprintf("[%s]:%d", remoteV6.String(), port)
	}
	if remoteV4.IsValid() {
		return fmt.Sprintf("%s:%d", remoteV4.String(), port)
	}
	if n.Endpoint.Domain != "" {
		return fmt.Sprintf("%s:%d", n.Endpoint.Domain, port)
	}
	return ""
}

// HasEndpoint reports whether this node has any reachable endpoint (ipv4, ipv6, or domain).
func (n *Node) HasEndpoint() bool {
	return n.Endpoint.IPv4 != "" || n.Endpoint.IPv6 != "" || n.Endpoint.Domain != ""
}

// HasIPv4 reports whether this node has an IPv4 endpoint address.
func (n *Node) HasIPv4() bool { return n.EndpointIPv4().IsValid() }

// HasIPv6 reports whether this node has an IPv6 endpoint address.
func (n *Node) HasIPv6() bool { return n.EndpointIPv6().IsValid() }

// EffectivePrefsrc4 returns the IPv4 prefsrc for underlay routes.
// If underlay.prefsrc4 is empty, defaults to the node's own endpoint IPv4.
func (n *Node) EffectivePrefsrc4() string {
	if n.Underlay == nil {
		return ""
	}
	if n.Underlay.Prefsrc4 != "" {
		return n.Underlay.Prefsrc4
	}
	if v4 := n.EndpointIPv4(); v4.IsValid() {
		return v4.String()
	}
	return ""
}

// EffectivePrefsrc6 returns the IPv6 prefsrc for underlay routes.
// If underlay.prefsrc6 is empty, defaults to the node's own endpoint IPv6.
func (n *Node) EffectivePrefsrc6() string {
	if n.Underlay == nil {
		return ""
	}
	if n.Underlay.Prefsrc6 != "" {
		return n.Underlay.Prefsrc6
	}
	if v6 := n.EndpointIPv6(); v6.IsValid() {
		return v6.String()
	}
	return ""
}

// EffectiveWGPort returns the WireGuard listen port for this node,
// falling back to the global default if not overridden.
func (n *Node) EffectiveWGPort(globalPort int) int {
	if n.WGListPort != 0 {
		return n.WGListPort
	}
	return globalPort
}

// Load reads and parses a meshctl inventory YAML file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	return Parse(data)
}

// Parse decodes raw YAML bytes into a Config and validates it.
func Parse(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing YAML: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate checks the config for logical errors.
func (c *Config) Validate() error {
	if err := c.validateGlobal(); err != nil {
		return err
	}
	if err := c.validateNodes(); err != nil {
		return err
	}
	if err := c.validatePeerRefs(); err != nil {
		return err
	}
	return nil
}

func (c *Config) validateGlobal() error {
	g := &c.Global
	if g.LinkLocalV4Range == "" {
		g.LinkLocalV4Range = "169.254.0.0/16"
	}
	if g.LinkLocalV4PrefLen == 0 {
		g.LinkLocalV4PrefLen = 31
	}
	if g.WGListenPort == 0 {
		g.WGListenPort = 51820
	}
	if g.OSPFArea == "" {
		g.OSPFArea = "0.0.0.0"
	}
	if g.OSPFHello == 0 {
		g.OSPFHello = 10
	}
	if g.OSPFDead == 0 {
		g.OSPFDead = 40
	}
	if g.ProbePort == 0 {
		g.ProbePort = 9473
	}
	if g.ProbeInterval == 0 {
		g.ProbeInterval = 30 * time.Second
	}
	if g.ProbeTimeout == 0 {
		g.ProbeTimeout = 5 * time.Second
	}
	if g.ProbeFailThreshold == 0 {
		g.ProbeFailThreshold = 3
	}
	if g.PenaltyCost == 0 {
		g.PenaltyCost = 65535
	}
	if g.EWMAAlpha == 0 {
		g.EWMAAlpha = 0.3
	}
	if g.OutputDir == "" {
		g.OutputDir = "./output"
	}
	if g.IGPTable4 == "" {
		g.IGPTable4 = "igptable4"
	}
	if g.IGPTable6 == "" {
		g.IGPTable6 = "igptable6"
	}
	if g.WGIfacePrefix == "" {
		g.WGIfacePrefix = "igp-"
	}
	if g.BandwidthThreshold == 0 {
		g.BandwidthThreshold = 300
	}
	if g.ReferenceBandwidth == 0 {
		g.ReferenceBandwidth = 3000
	}
	if len(g.CostBands) == 0 {
		g.CostBands = defaultCostBands()
	}
	if err := validateCostBands(g.CostBands); err != nil {
		return err
	}
	return nil
}

func defaultCostBands() []CostBandDef {
	return []CostBandDef{
		{Up: 0, Down: 0, Cost: 20, Hold: 5},
		{Up: 4 * time.Millisecond, Down: 2 * time.Millisecond, Cost: 50, Hold: 5},
		{Up: 12 * time.Millisecond, Down: 8 * time.Millisecond, Cost: 100, Hold: 5},
		{Up: 30 * time.Millisecond, Down: 20 * time.Millisecond, Cost: 200, Hold: 5},
		{Up: 60 * time.Millisecond, Down: 40 * time.Millisecond, Cost: 500, Hold: 5},
	}
}

// validateCostBands checks that cost bands are well-formed:
// - at least one band
// - up thresholds are strictly increasing
// - up >= down for each band (hysteresis requires down < up, except band 0)
// - hold count > 0
// - costs are strictly increasing
func validateCostBands(bands []CostBandDef) error {
	if len(bands) == 0 {
		return fmt.Errorf("cost_bands: at least one band is required")
	}
	for i, b := range bands {
		if b.Hold <= 0 {
			return fmt.Errorf("cost_bands[%d]: hold must be > 0", i)
		}
		if i > 0 && b.Up < b.Down {
			return fmt.Errorf("cost_bands[%d]: up threshold (%v) must be >= down threshold (%v)", i, b.Up, b.Down)
		}
		if i > 0 && b.Up <= bands[i-1].Up {
			return fmt.Errorf("cost_bands[%d]: up threshold (%v) must be > previous band up (%v)", i, b.Up, bands[i-1].Up)
		}
		if i > 0 && b.Cost <= bands[i-1].Cost {
			return fmt.Errorf("cost_bands[%d]: cost (%d) must be > previous band cost (%d)", i, b.Cost, bands[i-1].Cost)
		}
	}
	return nil
}

func (c *Config) validateNodes() error {
	if len(c.Nodes) == 0 {
		return fmt.Errorf("nodes: at least one node is required")
	}
	seen := make(map[string]bool)
	loopbacks := make(map[string]bool)
	for i := range c.Nodes {
		n := &c.Nodes[i]
		if n.Name == "" {
			return fmt.Errorf("nodes[%d]: name is required", i)
		}
		if !isValidNodeName(n.Name) {
			return fmt.Errorf("node %q: name must start with a letter; allowed: a-z, 0-9, hyphen, underscore, comma, dot", n.Name)
		}
		if seen[n.Name] {
			return fmt.Errorf("nodes: duplicate name %q", n.Name)
		}
		seen[n.Name] = true

		switch n.Type {
		case NodeTypeLinux, NodeTypeRouterOS, NodeTypeStatic:
		default:
			return fmt.Errorf("node %s: invalid type %q (must be linux, routeros, or static)", n.Name, n.Type)
		}

		// Endpoint is optional: nodes behind NAT may have no public IP.
		// Link validation ensures at least one side of each link has an endpoint.
		if n.Endpoint.IPv4 != "" {
			if _, err := netip.ParseAddr(n.Endpoint.IPv4); err != nil {
				return fmt.Errorf("node %s: endpoint.ipv4 %q is not a valid IPv4 address", n.Name, n.Endpoint.IPv4)
			}
		}
		if n.Endpoint.IPv6 != "" {
			if _, err := netip.ParseAddr(n.Endpoint.IPv6); err != nil {
				return fmt.Errorf("node %s: endpoint.ipv6 %q is not a valid IPv6 address", n.Name, n.Endpoint.IPv6)
			}
		}
		if n.Loopback == "" {
			return fmt.Errorf("node %s: loopback is required", n.Name)
		}
		if _, err := netip.ParseAddr(n.Loopback); err != nil {
			return fmt.Errorf("node %s: invalid loopback %q: %w", n.Name, n.Loopback, err)
		}
		if loopbacks[n.Loopback] {
			return fmt.Errorf("node %s: duplicate loopback %s", n.Name, n.Loopback)
		}
		loopbacks[n.Loopback] = true

		if n.PubKey == "" {
			return fmt.Errorf("node %s: pubkey is required", n.Name)
		}

		// Validate bandwidth (must be positive if set).
		if n.Bandwidth < 0 {
			return fmt.Errorf("node %s: bandwidth must be positive", n.Name)
		}

		// Validate cost_mode.
		switch n.CostModeSetting {
		case "", CostModeProbe, CostModeStatic:
		default:
			return fmt.Errorf("node %s: invalid cost_mode %q (must be probe or static)", n.Name, n.CostModeSetting)
		}

		// static_cost is required when cost_mode is "static".
		if n.CostModeSetting == CostModeStatic && n.StaticCost == nil {
			return fmt.Errorf("node %s: static_cost is required when cost_mode is static", n.Name)
		}

		// Validate wg_peer_port uniqueness: no two nodes may share the same
		// wg_peer_port value because it would cause listen-port collisions
		// on any node that peers with both of them.
		if n.WGPeerPort != 0 {
			for j := 0; j < i; j++ {
				if c.Nodes[j].WGPeerPort == n.WGPeerPort {
					return fmt.Errorf("node %s: wg_peer_port %d conflicts with node %s",
						n.Name, n.WGPeerPort, c.Nodes[j].Name)
				}
			}
		}

		// Validate underlay prefsrc: must be an IP, an interface name, or "auto".
		// We only reject values that look like IPs but fail to parse.
		if n.Underlay != nil {
			if v := n.Underlay.Prefsrc4; v != "" && v != "auto" {
				if addr, err := netip.ParseAddr(v); err == nil && !addr.Is4() {
					return fmt.Errorf("node %s: underlay prefsrc4 %q is not an IPv4 address", n.Name, v)
				}
				// If it doesn't parse as IP, it's treated as an interface name — valid.
			}
			if v := n.Underlay.Prefsrc6; v != "" && v != "auto" {
				if addr, err := netip.ParseAddr(v); err == nil && !addr.Is6() {
					return fmt.Errorf("node %s: underlay prefsrc6 %q is not an IPv6 address", n.Name, v)
				}
			}
		}
	}

	// Validate and auto-assign node_id.
	// Collect explicitly set IDs first.
	usedIDs := make(map[int]string) // id → node name
	for i := range c.Nodes {
		n := &c.Nodes[i]
		if n.NodeID < 0 {
			return fmt.Errorf("node %s: node_id must be positive", n.Name)
		}
		if n.NodeID > 0 {
			if other, ok := usedIDs[n.NodeID]; ok {
				return fmt.Errorf("node %s: node_id %d conflicts with node %s", n.Name, n.NodeID, other)
			}
			usedIDs[n.NodeID] = n.Name
		}
	}

	// Auto-assign IDs to nodes without one, in alphabetical name order.
	var unassigned []int // indices into c.Nodes
	for i := range c.Nodes {
		if c.Nodes[i].NodeID == 0 {
			unassigned = append(unassigned, i)
		}
	}
	sort.Slice(unassigned, func(a, b int) bool {
		return c.Nodes[unassigned[a]].Name < c.Nodes[unassigned[b]].Name
	})
	nextID := 1
	for _, idx := range unassigned {
		for usedIDs[nextID] != "" {
			nextID++
		}
		c.Nodes[idx].NodeID = nextID
		usedIDs[nextID] = c.Nodes[idx].Name
		nextID++
	}

	return nil
}

// validatePeerRefs checks that peers_with entries reference existing nodes.
func (c *Config) validatePeerRefs() error {
	names := make(map[string]bool)
	for _, n := range c.Nodes {
		names[n.Name] = true
	}
	for _, n := range c.Nodes {
		for _, p := range n.PeersWith {
			if !names[p] {
				return fmt.Errorf("node %s: peers_with references unknown node %q", n.Name, p)
			}
			if p == n.Name {
				return fmt.Errorf("node %s: peers_with cannot reference self", n.Name)
			}
		}
	}
	return nil
}

// NodeByName returns the node with the given name, or nil.
func (c *Config) NodeByName(name string) *Node {
	for i := range c.Nodes {
		if c.Nodes[i].Name == name {
			return &c.Nodes[i]
		}
	}
	return nil
}

// isValidNodeName checks that a node name contains only safe characters.
// Node names are used in file paths (output/<name>/), interface names, and
// BIRD config identifiers. Allowing arbitrary characters would enable path
// traversal (e.g. "../../../etc") or config injection.
func isValidNodeName(name string) bool {
	if len(name) == 0 || len(name) > 63 {
		return false
	}
	// Must start with a letter.
	first := name[0]
	if !((first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z')) {
		return false
	}
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == ',' || c == '.') {
			return false
		}
	}
	return true
}
