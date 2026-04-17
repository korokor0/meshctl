package config

import (
	"testing"
	"time"
)

func TestParse_ValidConfig(t *testing.T) {
	yaml := []byte(`
global:

  wg_listen_port: 51820
  probe_interval: 30s

nodes:
  - name: hk-core
    type: linux
    endpoint:
      ipv6: "2001:db8::1"
    loopback: 10.200.255.1
    pubkey: "aB3dTestKey1="
  - name: jp-relay
    type: linux
    endpoint:
      ipv4: "198.51.100.5"
    loopback: 10.200.255.3
    pubkey: "gH9iTestKey2="

link_policy:
  mode: full
`)
	cfg, err := Parse(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(cfg.Nodes))
	}
	if cfg.Global.WGListenPort != 51820 {
		t.Errorf("expected wg port 51820, got %d", cfg.Global.WGListenPort)
	}
	if cfg.Global.ProbeInterval != 30*time.Second {
		t.Errorf("expected probe interval 30s, got %v", cfg.Global.ProbeInterval)
	}
}

func TestParse_Defaults(t *testing.T) {
	yaml := []byte(`
global:


nodes:
  - name: node1
    type: linux
    endpoint:
      ipv4: "1.2.3.4"
    loopback: 10.200.255.1
    pubkey: "key1="

link_policy:
  mode: full
`)
	cfg, err := Parse(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	g := cfg.Global
	if g.WGListenPort != 51820 {
		t.Errorf("default wg port: got %d", g.WGListenPort)
	}
	if g.ProbePort != 9473 {
		t.Errorf("default probe port: got %d", g.ProbePort)
	}
	if g.PenaltyCost != 65535 {
		t.Errorf("default penalty cost: got %d", g.PenaltyCost)
	}
	if len(g.CostBands) != 5 {
		t.Errorf("default cost bands: got %d", len(g.CostBands))
	}
}

func TestParse_Errors(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{
			name: "no nodes",
			yaml: `
global:

nodes: []
`,
		},
		{
			name: "duplicate node name",
			yaml: `
global:

nodes:
  - name: dup
    type: linux
    endpoint:
      ipv4: "1.2.3.4"
    loopback: 10.0.0.1
    pubkey: "k1="
  - name: dup
    type: linux
    endpoint:
      ipv4: "1.2.3.5"
    loopback: 10.0.0.2
    pubkey: "k2="
`,
		},
		{
			name: "invalid node type",
			yaml: `
global:

nodes:
  - name: bad
    type: windows
    endpoint:
      ipv4: "1.2.3.4"
    loopback: 10.0.0.1
    pubkey: "k="
`,
		},
		{
			name: "duplicate loopback",
			yaml: `
global:

nodes:
  - name: a
    type: linux
    endpoint:
      ipv4: "1.2.3.4"
    loopback: 10.0.0.1
    pubkey: "k1="
  - name: b
    type: linux
    endpoint:
      ipv4: "1.2.3.5"
    loopback: 10.0.0.1
    pubkey: "k2="
`,
		},
		{
			name: "missing pubkey",
			yaml: `
global:

nodes:
  - name: nopk
    type: linux
    endpoint:
      ipv4: "1.2.3.4"
    loopback: 10.0.0.1
`,
		},
		{
			name: "peers_with unknown node",
			yaml: `
global:

nodes:
  - name: a
    type: static
    endpoint:
      ipv4: "1.2.3.4"
    loopback: 10.0.0.1
    pubkey: "k="
    peers_with:
      - nonexistent
`,
		},
		{
			name: "peers_with self",
			yaml: `
global:

nodes:
  - name: a
    type: static
    endpoint:
      ipv4: "1.2.3.4"
    loopback: 10.0.0.1
    pubkey: "k="
    peers_with:
      - a
`,
		},
		{
			name: "invalid cost_mode",
			yaml: `
global:

nodes:
  - name: a
    type: routeros
    endpoint:
      ipv4: "1.2.3.4"
    loopback: 10.0.0.1
    pubkey: "k="
    cost_mode: "magic"
`,
		},
		{
			name: "static cost_mode without static_cost",
			yaml: `
global:

nodes:
  - name: a
    type: routeros
    endpoint:
      ipv4: "1.2.3.4"
    loopback: 10.0.0.1
    pubkey: "k="
    cost_mode: static
`,
		},
		{
			name: "duplicate wg_peer_port",
			yaml: `
global:

nodes:
  - name: a
    type: linux
    endpoint:
      ipv4: "1.2.3.4"
    loopback: 10.0.0.1
    pubkey: "k1="
    wg_peer_port: 60001
  - name: b
    type: linux
    endpoint:
      ipv4: "1.2.3.5"
    loopback: 10.0.0.2
    pubkey: "k2="
    wg_peer_port: 60001
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestNodeByName(t *testing.T) {
	cfg := &Config{
		Nodes: []Node{
			{Name: "alpha"},
			{Name: "beta"},
		},
	}
	if n := cfg.NodeByName("alpha"); n == nil || n.Name != "alpha" {
		t.Error("expected to find alpha")
	}
	if n := cfg.NodeByName("gamma"); n != nil {
		t.Error("expected nil for missing node")
	}
}

func TestCostModeFields(t *testing.T) {
	yaml := []byte(`
global:

nodes:
  - name: fat1
    type: linux
    endpoint:
      ipv4: "1.2.3.4"
    loopback: 10.0.0.1
    pubkey: "k1="
  - name: thin1
    type: routeros
    endpoint:
      ipv4: "1.2.3.5"
    loopback: 10.0.0.2
    pubkey: "k2="
    cost_mode: static
    static_cost: 150
  - name: thin2
    type: routeros
    endpoint:
      ipv4: "1.2.3.6"
    loopback: 10.0.0.3
    pubkey: "k3="
    cost_mode: probe
    static_cost: 200
link_policy:
  mode: full
`)
	cfg, err := Parse(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// fat1: default probe mode.
	fat1 := cfg.NodeByName("fat1")
	if fat1.EffectiveCostMode() != CostModeProbe {
		t.Errorf("fat1: expected probe mode, got %s", fat1.EffectiveCostMode())
	}
	if _, ok := fat1.EffectiveStaticCost(); ok {
		t.Error("fat1: should not have static cost")
	}

	// thin1: static cost mode.
	thin1 := cfg.NodeByName("thin1")
	if thin1.EffectiveCostMode() != CostModeStatic {
		t.Errorf("thin1: expected static mode, got %s", thin1.EffectiveCostMode())
	}
	if c, ok := thin1.EffectiveStaticCost(); !ok || c != 150 {
		t.Errorf("thin1: expected static cost 150, got %d (ok=%v)", c, ok)
	}

	// thin2: probe mode with fallback cost.
	thin2 := cfg.NodeByName("thin2")
	if thin2.EffectiveCostMode() != CostModeProbe {
		t.Errorf("thin2: expected probe mode, got %s", thin2.EffectiveCostMode())
	}
	if c, ok := thin2.EffectiveStaticCost(); !ok || c != 200 {
		t.Errorf("thin2: expected static cost 200 (for fallback), got %d (ok=%v)", c, ok)
	}
}

func TestNodeID_AutoAssign(t *testing.T) {
	yaml := []byte(`
global:

nodes:
  - name: charlie
    type: linux
    endpoint:
      ipv4: "1.2.3.3"
    loopback: 10.0.0.3
    pubkey: "k3="
  - name: alpha
    type: linux
    endpoint:
      ipv4: "1.2.3.1"
    loopback: 10.0.0.1
    pubkey: "k1="
  - name: bravo
    type: linux
    endpoint:
      ipv4: "1.2.3.2"
    loopback: 10.0.0.2
    pubkey: "k2="
link_policy:
  mode: full
`)
	cfg, err := Parse(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Auto-assigned alphabetically: alpha=1, bravo=2, charlie=3.
	if id := cfg.NodeByName("alpha").NodeID; id != 1 {
		t.Errorf("alpha: expected node_id 1, got %d", id)
	}
	if id := cfg.NodeByName("bravo").NodeID; id != 2 {
		t.Errorf("bravo: expected node_id 2, got %d", id)
	}
	if id := cfg.NodeByName("charlie").NodeID; id != 3 {
		t.Errorf("charlie: expected node_id 3, got %d", id)
	}
}

func TestNodeID_ExplicitAndAutoMixed(t *testing.T) {
	yaml := []byte(`
global:

nodes:
  - name: alpha
    type: linux
    endpoint:
      ipv4: "1.2.3.1"
    loopback: 10.0.0.1
    pubkey: "k1="
    node_id: 5
  - name: bravo
    type: linux
    endpoint:
      ipv4: "1.2.3.2"
    loopback: 10.0.0.2
    pubkey: "k2="
  - name: charlie
    type: linux
    endpoint:
      ipv4: "1.2.3.3"
    loopback: 10.0.0.3
    pubkey: "k3="
    node_id: 1
link_policy:
  mode: full
`)
	cfg, err := Parse(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// alpha=5 (explicit), charlie=1 (explicit), bravo gets next available (2).
	if id := cfg.NodeByName("alpha").NodeID; id != 5 {
		t.Errorf("alpha: expected 5, got %d", id)
	}
	if id := cfg.NodeByName("charlie").NodeID; id != 1 {
		t.Errorf("charlie: expected 1, got %d", id)
	}
	if id := cfg.NodeByName("bravo").NodeID; id != 2 {
		t.Errorf("bravo: expected 2, got %d", id)
	}
}

func TestNodeID_DuplicateError(t *testing.T) {
	yaml := []byte(`
global:

nodes:
  - name: a
    type: linux
    endpoint:
      ipv4: "1.2.3.1"
    loopback: 10.0.0.1
    pubkey: "k1="
    node_id: 3
  - name: b
    type: linux
    endpoint:
      ipv4: "1.2.3.2"
    loopback: 10.0.0.2
    pubkey: "k2="
    node_id: 3
`)
	_, err := Parse(yaml)
	if err == nil {
		t.Fatal("expected error for duplicate node_id")
	}
}

func TestNodeID_NegativeError(t *testing.T) {
	yaml := []byte(`
global:

nodes:
  - name: a
    type: linux
    endpoint:
      ipv4: "1.2.3.1"
    loopback: 10.0.0.1
    pubkey: "k1="
    node_id: -1
`)
	_, err := Parse(yaml)
	if err == nil {
		t.Fatal("expected error for negative node_id")
	}
}

func TestParse_NodeWithoutEndpoint(t *testing.T) {
	yaml := []byte(`
global:

nodes:
  - name: nat-node
    type: linux
    loopback: 10.0.0.1
    pubkey: "k1="
    peers_with: [hub]
  - name: hub
    type: linux
    endpoint:
      ipv4: "1.2.3.4"
    loopback: 10.0.0.2
    pubkey: "k2="

link_policy:
  mode: full
`)
	cfg, err := Parse(yaml)
	if err != nil {
		t.Fatalf("node without endpoint should be valid: %v", err)
	}
	natNode := cfg.NodeByName("nat-node")
	if natNode.HasEndpoint() {
		t.Error("nat-node should not have endpoint")
	}
	hub := cfg.NodeByName("hub")
	if !hub.HasEndpoint() {
		t.Error("hub should have endpoint")
	}
}

func TestEffectiveWGPort(t *testing.T) {
	n := &Node{Name: "test"}
	if got := n.EffectiveWGPort(51820); got != 51820 {
		t.Errorf("expected global fallback 51820, got %d", got)
	}
	n.WGListPort = 13231
	if got := n.EffectiveWGPort(51820); got != 13231 {
		t.Errorf("expected override 13231, got %d", got)
	}
}
