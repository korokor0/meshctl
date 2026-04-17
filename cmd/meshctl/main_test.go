package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// minimalYAML is a small valid inventory for testing CLI commands.
const minimalYAML = `
global:
  linklocal_v4_range: "169.254.0.0/16"
  linklocal_v4_prefix_len: 31
  wg_listen_port: 51820
  wg_persistent_keepalive: 25
  ospf_area: 0.0.0.0
  ospf_hello: 10
  ospf_dead: 40
  output_dir: "%s"

nodes:
  - name: node-a
    type: linux
    node_id: 1
    endpoint:
      ipv4: "1.2.3.4"
    loopback: 10.200.255.1
    pubkey: "dGVzdGtleTE="

  - name: node-b
    type: routeros
    node_id: 2
    endpoint:
      ipv4: "5.6.7.8"
    loopback: 10.200.255.2
    pubkey: "dGVzdGtleTI="
    cost_mode: static
    static_cost: 150

link_policy:
  mode: full
`

func writeTestConfig(t *testing.T, outputDir string) string {
	t.Helper()
	dir := t.TempDir()
	content := []byte(testYAML(outputDir))
	path := filepath.Join(dir, "meshctl.yaml")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func testYAML(outputDir string) string {
	return fmt.Sprintf(minimalYAML, outputDir)
}

func TestGenerateCmd(t *testing.T) {
	outputDir := t.TempDir()
	configPath := writeTestConfig(t, outputDir)

	cmd := generateCmd()
	cmd.SetArgs([]string{"--config", configPath})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("generate command failed: %v", err)
	}

	// Check output files were created.
	for _, sub := range []string{"node-a", "node-b"} {
		nodeDir := filepath.Join(outputDir, sub)
		entries, err := os.ReadDir(nodeDir)
		if err != nil {
			t.Fatalf("expected output dir for %s: %v", sub, err)
		}
		if len(entries) == 0 {
			t.Errorf("expected generated files for %s, got none", sub)
		}
	}

	// Linux node should have wireguard.json and bird-meshctl.conf.
	for _, f := range []string{"wireguard.json", "bird-meshctl.conf"} {
		path := filepath.Join(outputDir, "node-a", f)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected %s for linux node: %v", f, err)
		}
	}

	// RouterOS node should have wireguard.rsc and ospf.rsc.
	for _, f := range []string{"wireguard.rsc", "ospf.rsc", "full-setup.rsc"} {
		path := filepath.Join(outputDir, "node-b", f)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected %s for routeros node: %v", f, err)
		}
	}
}

func TestValidateCmd(t *testing.T) {
	outputDir := t.TempDir()
	configPath := writeTestConfig(t, outputDir)

	cmd := validateCmd()
	cmd.SetArgs([]string{"--config", configPath})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("validate command failed: %v", err)
	}
}

func TestValidateCmd_InvalidConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	os.WriteFile(path, []byte("not valid yaml: ["), 0o644)

	cmd := validateCmd()
	cmd.SetArgs([]string{"--config", path})

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected validate to fail with invalid YAML")
	}
}

func TestShowMeshCmd(t *testing.T) {
	outputDir := t.TempDir()
	configPath := writeTestConfig(t, outputDir)

	cmd := showMeshCmd()
	cmd.SetArgs([]string{"--config", configPath})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("show-mesh command failed: %v", err)
	}
}

func TestDiffCmd_NoExistingOutput(t *testing.T) {
	outputDir := t.TempDir()
	configPath := writeTestConfig(t, outputDir)

	cmd := diffCmd()
	cmd.SetArgs([]string{"--config", configPath})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("diff command failed: %v", err)
	}
}

func TestDiffCmd_NoChanges(t *testing.T) {
	outputDir := t.TempDir()
	configPath := writeTestConfig(t, outputDir)

	// First generate.
	gen := generateCmd()
	gen.SetArgs([]string{"--config", configPath})
	if err := gen.Execute(); err != nil {
		t.Fatalf("generate failed: %v", err)
	}

	// Then diff — should show no changes.
	cmd := diffCmd()
	cmd.SetArgs([]string{"--config", configPath})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("diff command failed: %v", err)
	}
}

func TestGenerateCmd_StaticNode(t *testing.T) {
	dir := t.TempDir()
	outputDir := t.TempDir()
	yaml := fmt.Sprintf(`
global:
  linklocal_v4_range: "169.254.0.0/16"
  linklocal_v4_prefix_len: 31
  wg_listen_port: 51820
  wg_persistent_keepalive: 25
  ospf_area: 0.0.0.0
  ospf_hello: 10
  ospf_dead: 40
  output_dir: "%s"

nodes:
  - name: alpha
    type: linux
    node_id: 1
    endpoint:
      ipv4: "1.2.3.4"
    loopback: 10.200.255.1
    pubkey: "a2V5MQ=="

  - name: beta
    type: static
    node_id: 2
    endpoint:
      ipv4: "5.6.7.8"
    loopback: 10.200.255.2
    pubkey: "a2V5Mg=="

link_policy:
  mode: full
`, outputDir)

	path := filepath.Join(dir, "meshctl.yaml")
	os.WriteFile(path, []byte(yaml), 0o644)

	cmd := generateCmd()
	cmd.SetArgs([]string{"--config", path})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("generate with static node failed: %v", err)
	}

	// Static node should have wireguard.conf.snippet.
	snippetPath := filepath.Join(outputDir, "beta", "wireguard.conf.snippet")
	if _, err := os.Stat(snippetPath); err != nil {
		t.Errorf("expected wireguard.conf.snippet for static node: %v", err)
	}
}
