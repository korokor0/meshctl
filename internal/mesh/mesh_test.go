package mesh

import (
	"fmt"
	"testing"

	"github.com/honoka/meshctl/internal/config"
)

func testConfig() *config.Config {
	return &config.Config{
		Global: config.Global{
			LinkLocalV4Range:   "169.254.0.0/16",
			LinkLocalV4PrefLen: 31,
			WGListenPort:       51820,
		},
		Nodes: []config.Node{
			{Name: "hk-core", Type: config.NodeTypeLinux, Endpoint: config.EndpointDef{IPv6: "2001:db8::1"}, Loopback: "10.200.255.1", PubKey: "k1=", NodeID: 1},
			{Name: "hk-edge", Type: config.NodeTypeRouterOS, Endpoint: config.EndpointDef{IPv6: "2001:db8::2"}, Loopback: "10.200.255.2", PubKey: "k2=", WGListPort: 13231, NodeID: 2},
			{Name: "jp-relay", Type: config.NodeTypeLinux, Endpoint: config.EndpointDef{IPv4: "198.51.100.5"}, Loopback: "10.200.255.3", PubKey: "k3=", NodeID: 3},
		},
		LinkPolicy: config.LinkPolicy{Mode: "full"},
	}
}

func TestComputeLinks_FullMesh(t *testing.T) {
	cfg := testConfig()
	links, err := ComputeLinks(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 3 nodes → 3 links in full mesh.
	if len(links) != 3 {
		t.Fatalf("expected 3 links, got %d", len(links))
	}

	// Verify sorted order: NodeA < NodeB.
	for _, l := range links {
		if l.NodeA >= l.NodeB {
			t.Errorf("link not sorted: %s >= %s", l.NodeA, l.NodeB)
		}
	}
}

func TestComputeLinks_LinkModes(t *testing.T) {
	cfg := testConfig()
	links, _ := ComputeLinks(cfg)

	for _, l := range links {
		aType := cfg.NodeByName(l.NodeA).Type
		bType := cfg.NodeByName(l.NodeB).Type
		bothLinux := aType == config.NodeTypeLinux && bType == config.NodeTypeLinux

		if bothLinux && l.Mode != LinkModeFe80 {
			t.Errorf("link %s-%s: expected fe80, got v4ll", l.NodeA, l.NodeB)
		}
		if !bothLinux && l.Mode != LinkModeV4LL {
			t.Errorf("link %s-%s: expected v4ll, got fe80", l.NodeA, l.NodeB)
		}
	}
}

func TestComputeLinks_PeersWith(t *testing.T) {
	cfg := &config.Config{
		Global: config.Global{
			LinkLocalV4Range: "169.254.0.0/16",
			WGListenPort:     51820,
		},
		Nodes: []config.Node{
			{Name: "a", Type: config.NodeTypeLinux, Endpoint: config.EndpointDef{IPv4: "1.0.0.1"}, Loopback: "10.0.0.1", PubKey: "k1="},
			{Name: "b", Type: config.NodeTypeLinux, Endpoint: config.EndpointDef{IPv4: "1.0.0.2"}, Loopback: "10.0.0.2", PubKey: "k2="},
			{Name: "c", Type: config.NodeTypeStatic, Endpoint: config.EndpointDef{IPv4: "1.0.0.3"}, Loopback: "10.0.0.3", PubKey: "k3=",
				PeersWith: []string{"a"}},
		},
		LinkPolicy: config.LinkPolicy{Mode: "full"},
	}
	links, _ := ComputeLinks(cfg)

	// c peers_with only a → links: a-b, a-c. No b-c.
	if len(links) != 2 {
		t.Fatalf("expected 2 links, got %d", len(links))
	}
	for _, l := range links {
		if l.NodeA == "b" && l.NodeB == "c" {
			t.Error("unexpected link b-c; c only peers_with a")
		}
	}
}

func TestAssignAddresses(t *testing.T) {
	cfg := testConfig()
	links, _ := ComputeLinks(cfg)
	if err := AssignAddresses(links, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, l := range links {
		// ALL links get V4LL addresses (OSPFv3 IPv4 AF needs IPv4 nexthop).
		if !l.AddrA.IsValid() || !l.AddrB.IsValid() {
			t.Errorf("link %s-%s: missing addresses", l.NodeA, l.NodeB)
			continue
		}
		nodeA := cfg.NodeByName(l.NodeA)
		nodeB := cfg.NodeByName(l.NodeB)
		if l.AddrA.Bits() != 32 {
			t.Errorf("link %s-%s: AddrA prefix len %d, want 32", l.NodeA, l.NodeB, l.AddrA.Bits())
		}
		if l.AddrB.Bits() != 32 {
			t.Errorf("link %s-%s: AddrB prefix len %d, want 32", l.NodeA, l.NodeB, l.AddrB.Bits())
		}
		expectA := fmt.Sprintf("169.254.0.%d", nodeA.NodeID)
		expectB := fmt.Sprintf("169.254.0.%d", nodeB.NodeID)
		if l.AddrA.Addr().String() != expectA {
			t.Errorf("link %s-%s: AddrA=%s, want %s", l.NodeA, l.NodeB, l.AddrA.Addr(), expectA)
		}
		if l.AddrB.Addr().String() != expectB {
			t.Errorf("link %s-%s: AddrB=%s, want %s", l.NodeA, l.NodeB, l.AddrB.Addr(), expectB)
		}
	}
}

func TestAssignAddresses_Deterministic(t *testing.T) {
	cfg := testConfig()

	links1, _ := ComputeLinks(cfg)
	AssignAddresses(links1, cfg)

	links2, _ := ComputeLinks(cfg)
	AssignAddresses(links2, cfg)

	for i := range links1 {
		if links1[i].AddrA != links2[i].AddrA || links1[i].AddrB != links2[i].AddrB {
			t.Errorf("non-deterministic addressing for %s-%s", links1[i].NodeA, links1[i].NodeB)
		}
	}
}

func TestLinksForNode(t *testing.T) {
	cfg := testConfig()
	links, _ := ComputeLinks(cfg)
	nodeLinks := LinksForNode(links, "hk-core")
	if len(nodeLinks) != 2 {
		t.Errorf("expected 2 links for hk-core, got %d", len(nodeLinks))
	}
}

func TestLinkPeerName(t *testing.T) {
	l := Link{NodeA: "alpha", NodeB: "beta"}
	if l.PeerName("alpha") != "beta" {
		t.Error("expected beta")
	}
	if l.PeerName("beta") != "alpha" {
		t.Error("expected alpha")
	}
}

func TestComputeLinks_NoEndpointOneNode(t *testing.T) {
	cfg := &config.Config{
		Global: config.Global{
			LinkLocalV4Range: "169.254.0.0/16",
			WGListenPort:     51820,
		},
		Nodes: []config.Node{
			{Name: "a", Type: config.NodeTypeLinux, Endpoint: config.EndpointDef{IPv4: "1.0.0.1"}, Loopback: "10.0.0.1", PubKey: "k1=", NodeID: 1},
			{Name: "b", Type: config.NodeTypeLinux, Loopback: "10.0.0.2", PubKey: "k2=", NodeID: 2}, // no endpoint
		},
		LinkPolicy: config.LinkPolicy{Mode: "full"},
	}
	links, err := ComputeLinks(cfg)
	if err != nil {
		t.Fatalf("unexpected error: one side has endpoint, should be ok: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("expected 1 link, got %d", len(links))
	}
}

func TestComputeLinks_NoEndpointBothNodes(t *testing.T) {
	cfg := &config.Config{
		Global: config.Global{
			LinkLocalV4Range: "169.254.0.0/16",
			WGListenPort:     51820,
		},
		Nodes: []config.Node{
			{Name: "a", Type: config.NodeTypeLinux, Loopback: "10.0.0.1", PubKey: "k1=", NodeID: 1}, // no endpoint
			{Name: "b", Type: config.NodeTypeLinux, Loopback: "10.0.0.2", PubKey: "k2=", NodeID: 2}, // no endpoint
		},
		LinkPolicy: config.LinkPolicy{Mode: "full"},
	}
	_, err := ComputeLinks(cfg)
	if err == nil {
		t.Fatal("expected error when both nodes have no endpoint")
	}
}

func TestFe80ForNode(t *testing.T) {
	tests := []struct {
		nodeID int
		want   string
	}{
		{1, "fe80::127:1"},
		{2, "fe80::127:2"},
		{10, "fe80::127:a"},
		{255, "fe80::127:ff"},
	}
	for _, tt := range tests {
		got := Fe80ForNode(tt.nodeID)
		if got != tt.want {
			t.Errorf("Fe80ForNode(%d) = %q, want %q", tt.nodeID, got, tt.want)
		}
	}
}
