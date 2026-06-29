package generate

import (
	"strings"
	"testing"

	"github.com/honoka/meshctl/internal/config"
	"github.com/honoka/meshctl/internal/mesh"
)

func testCfg() *config.Config {
	return &config.Config{
		Global: config.Global{
			LinkLocalV4Range:   "169.254.0.0/16",
			LinkLocalV4PrefLen: 31,
			WGListenPort:       51820,
			WGKeepAlive:        25,
			OSPFArea:           "0.0.0.0",
			OSPFHello:          10,
			OSPFDead:           40,
			ProbePort:          9473,
			PenaltyCost:        65535,
			OutputDir:          "./output",
			WGIfacePrefix:      "igp-",
			IGPTable4:          "igptable4",
			IGPTable6:          "igptable6",
		},
		Nodes: []config.Node{
			{Name: "hk-core", Type: config.NodeTypeLinux, Endpoint: config.EndpointDef{IPv4: "1.2.3.4", IPv6: "2001:db8::1"},
				Loopback: "10.200.255.1", PubKey: "aB3dTestKey1=", Announce: []string{"192.168.1.0/24"}, NodeID: 1},
			{Name: "hk-edge", Type: config.NodeTypeRouterOS, Endpoint: config.EndpointDef{IPv6: "2001:db8::2"},
				Loopback: "10.200.255.2", PubKey: "cD5fTestKey2=", WGListPort: 13231,
				Announce: []string{"192.168.2.0/24"}, NodeID: 2},
			{Name: "jp-relay", Type: config.NodeTypeLinux, Endpoint: config.EndpointDef{IPv4: "198.51.100.5"},
				Loopback: "10.200.255.3", PubKey: "gH9iTestKey3=", NodeID: 3},
			{Name: "friend-node", Type: config.NodeTypeStatic, Endpoint: config.EndpointDef{IPv4: "203.0.113.99"},
				Loopback: "10.200.255.10", PubKey: "eF7gTestKey4=",
				PeersWith: []string{"hk-core", "jp-relay"}, NodeID: 10},
		},
		LinkPolicy: config.LinkPolicy{Mode: "full"},
	}
}

func TestBuildWGPeers(t *testing.T) {
	cfg := testCfg()
	links, _ := mesh.ComputeLinks(cfg)
	mesh.AssignAddresses(links, cfg)

	peers := BuildWGPeers(cfg, "hk-core", links)

	// hk-core should peer with hk-edge, jp-relay, and friend-node.
	if len(peers) != 3 {
		t.Fatalf("expected 3 peers, got %d", len(peers))
	}

	// Check that peer names are present.
	names := make(map[string]bool)
	for _, p := range peers {
		names[p.Name] = true
	}
	for _, want := range []string{"hk-edge", "jp-relay", "friend-node"} {
		if !names[want] {
			t.Errorf("missing peer %s", want)
		}
	}
}

func TestBIRDGenerator_GenerateOSPF(t *testing.T) {
	cfg := testCfg()
	links, _ := mesh.ComputeLinks(cfg)
	mesh.AssignAddresses(links, cfg)

	gen, err := NewBIRDGenerator(cfg)
	if err != nil {
		t.Fatalf("new generator: %v", err)
	}

	node := cfg.NodeByName("hk-core")
	out, err := gen.GenerateOSPF(node, links)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	s := string(out)
	// hk-core peers with jp-relay (both linux → fe80) and hk-edge (routeros → v4ll).
	if !strings.Contains(s, "meshctl_ospf3_v4") {
		t.Error("expected OSPFv3 IPv4 AF block for fe80 links")
	}
	if !strings.Contains(s, "meshctl_ospf3_v6") {
		t.Error("expected OSPFv3 IPv6 AF block for fe80 links")
	}
	if !strings.Contains(s, "instance id 64") {
		t.Error("expected instance id 64 for OSPFv3 IPv4 AF")
	}
	if !strings.Contains(s, "meshctl_ospf2") {
		t.Error("expected OSPFv2 block for v4ll links")
	}
	if !strings.Contains(s, "igptable4") {
		t.Error("expected igptable4 reference")
	}
	if !strings.Contains(s, "igptable6") {
		t.Error("expected igptable6 reference")
	}
	if !strings.Contains(s, "igp-jp-relay") {
		t.Error("expected igp-jp-relay interface")
	}
}

func TestBIRDGenerator_GenerateWireguard(t *testing.T) {
	cfg := testCfg()
	links, _ := mesh.ComputeLinks(cfg)
	mesh.AssignAddresses(links, cfg)

	gen, err := NewBIRDGenerator(cfg)
	if err != nil {
		t.Fatalf("new generator: %v", err)
	}

	node := cfg.NodeByName("hk-core")
	peers := BuildWGPeers(cfg, "hk-core", links)
	out, err := gen.GenerateWireguard(node, peers)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	s := string(out)
	if !strings.Contains(s, "aB3dTestKey1=") && !strings.Contains(s, "cD5fTestKey2=") {
		// Should contain peer keys, not own key.
	}
	if !strings.Contains(s, "hk-core") {
		t.Error("expected node name in output")
	}
}

func TestRouterOSGenerator_GenerateFull(t *testing.T) {
	cfg := testCfg()
	links, _ := mesh.ComputeLinks(cfg)
	mesh.AssignAddresses(links, cfg)

	gen := NewRouterOSGenerator(cfg)
	node := cfg.NodeByName("hk-edge")
	peers := BuildWGPeers(cfg, "hk-edge", links)
	out, err := gen.GenerateFull(node, peers, links)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	s := string(out)
	if !strings.Contains(s, "wireguard") {
		t.Error("expected wireguard section")
	}
	if !strings.Contains(s, "ospf") {
		t.Error("expected ospf section")
	}
}

func TestStaticSnippetGenerator(t *testing.T) {
	cfg := testCfg()
	links, _ := mesh.ComputeLinks(cfg)
	mesh.AssignAddresses(links, cfg)

	gen := NewStaticSnippetGenerator(cfg)
	node := cfg.NodeByName("friend-node")
	peers := BuildWGPeers(cfg, "friend-node", links)
	out, err := gen.GenerateFull(node, peers, links)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	s := string(out)
	if !strings.Contains(s, "friend-node") {
		t.Error("expected node name in output")
	}
	if !strings.Contains(s, "static node") {
		t.Error("expected static node label")
	}
}

func TestWGInterfaceName(t *testing.T) {
	tests := []struct {
		prefix string
		peer   string
		want   string
	}{
		{"igp-", "hk-core", "igp-hk-core"},
		{"igp-", "jp-relay", "igp-jp-relay"},
		{"igp-", "very-long-peer-name", "igp-verylongpee"}, // strip symbols, then truncate
		{"wg-", "HKG", "wg-hkg"},                           // uppercase → lowercase
		{"igp-", "NYC", "igp-nyc"},
		{"igp-", "kskb,TW", "igp-kskb,tw"},                   // short enough, keep symbols
		{"igp-", "a.b,c-d_e.f,long-name", "igp-abcdeflongn"}, // strip symbols, then truncate
	}
	for _, tt := range tests {
		got := WGInterfaceName(tt.prefix, tt.peer)
		if got != tt.want {
			t.Errorf("WGInterfaceName(%q, %q) = %q, want %q", tt.prefix, tt.peer, got, tt.want)
		}
	}
}

func TestIfaceNameForPeer_Override(t *testing.T) {
	owner := &config.Node{
		Name:            "hk-edge",
		WGIfaceOverride: map[string]string{"hk-core": "wg-existing0"},
	}
	// Overridden peer uses the configured name.
	if got := IfaceNameForPeer("igp-", owner, "hk-core"); got != "wg-existing0" {
		t.Errorf("override: got %q, want %q", got, "wg-existing0")
	}
	// Non-overridden peer falls back to the derived name.
	if got := IfaceNameForPeer("igp-", owner, "jp-relay"); got != "igp-jp-relay" {
		t.Errorf("fallback: got %q, want %q", got, "igp-jp-relay")
	}
	// Nil owner falls back to the derived name.
	if got := IfaceNameForPeer("igp-", nil, "hk-core"); got != "igp-hk-core" {
		t.Errorf("nil owner: got %q, want %q", got, "igp-hk-core")
	}
}

// TestWGIfaceOverride_BIRD verifies that a per-peer interface override on a
// fat node flows into both the BIRD OSPF config and the agent's wireguard.json.
func TestWGIfaceOverride_BIRD(t *testing.T) {
	cfg := testCfg()
	// hk-core reuses an existing interface for its link to hk-edge.
	cfg.NodeByName("hk-core").WGIfaceOverride = map[string]string{"hk-edge": "wg-edge0"}

	links, _ := mesh.ComputeLinks(cfg)
	mesh.AssignAddresses(links, cfg)

	gen, err := NewBIRDGenerator(cfg)
	if err != nil {
		t.Fatalf("new generator: %v", err)
	}
	node := cfg.NodeByName("hk-core")

	ospf, err := gen.GenerateOSPF(node, links)
	if err != nil {
		t.Fatalf("generate OSPF: %v", err)
	}
	if s := string(ospf); !strings.Contains(s, `interface "wg-edge0"`) {
		t.Errorf("BIRD OSPF missing overridden interface name; got:\n%s", s)
	} else if strings.Contains(s, `interface "igp-hk-edge"`) {
		t.Error("BIRD OSPF still uses derived name for overridden link")
	}

	peers := BuildWGPeers(cfg, "hk-core", links)
	wg, err := gen.GenerateWireguard(node, peers)
	if err != nil {
		t.Fatalf("generate WG: %v", err)
	}
	if s := string(wg); !strings.Contains(s, `"interface": "wg-edge0"`) {
		t.Errorf("wireguard.json missing overridden interface name; got:\n%s", s)
	}
	// The non-overridden peer (jp-relay) keeps its derived name.
	if s := string(wg); !strings.Contains(s, `"interface": "igp-jp-relay"`) {
		t.Error("wireguard.json lost derived name for non-overridden peer")
	}
}

// TestCheckIfaceOverrides verifies that an override targeting an actual peer
// passes, while one targeting a real-but-non-adjacent node is rejected.
func TestCheckIfaceOverrides(t *testing.T) {
	cfg := testCfg()
	links, _ := mesh.ComputeLinks(cfg)
	mesh.AssignAddresses(links, cfg)

	// Valid: hk-edge actually peers with hk-core.
	cfg.NodeByName("hk-edge").WGIfaceOverride = map[string]string{"hk-core": "wireguard1"}
	if err := CheckIfaceOverrides(cfg, links); err != nil {
		t.Errorf("override to a real peer should pass, got: %v", err)
	}

	// Invalid: friend-node only peers_with hk-core and jp-relay, so it is not
	// a peer of hk-edge — the override is a silent no-op without this check.
	cfg.NodeByName("hk-edge").WGIfaceOverride = map[string]string{"friend-node": "wireguard1"}
	if err := CheckIfaceOverrides(cfg, links); err == nil {
		t.Error("override to a non-peer node should be rejected")
	}
}

// TestWGIfaceOverride_RouterOS verifies the override flows into .rsc output,
// so a manually-created RouterOS interface can be reused.
func TestWGIfaceOverride_RouterOS(t *testing.T) {
	cfg := testCfg()
	// hk-edge (RouterOS) reuses its existing interface for the link to hk-core.
	cfg.NodeByName("hk-edge").WGIfaceOverride = map[string]string{"hk-core": "wireguard1"}

	links, _ := mesh.ComputeLinks(cfg)
	mesh.AssignAddresses(links, cfg)

	gen := NewRouterOSGenerator(cfg)
	node := cfg.NodeByName("hk-edge")
	peers := BuildWGPeers(cfg, "hk-edge", links)

	out, err := gen.GenerateFull(node, peers, links)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `name="wireguard1"`) {
		t.Errorf(".rsc missing overridden interface name; got:\n%s", s)
	}
	if strings.Contains(s, `name="igp-hk-core"`) {
		t.Error(".rsc still uses derived name for overridden link")
	}
}
