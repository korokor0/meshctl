package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/honoka/meshctl/internal/config"
	"github.com/honoka/meshctl/internal/generate"
	"github.com/honoka/meshctl/internal/mesh"
	"github.com/honoka/meshctl/internal/psk"
)

func main() {
	root := &cobra.Command{
		Use:   "meshctl",
		Short: "Overlay mesh network config generator",
	}

	root.AddCommand(generateCmd())
	root.AddCommand(validateCmd())
	root.AddCommand(diffCmd())
	root.AddCommand(showMeshCmd())
	root.AddCommand(pskCmd())

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func generateCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Generate config files from inventory YAML",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}

			// Compute links and assign addresses.
			links, err := mesh.ComputeLinks(cfg)
			if err != nil {
				return fmt.Errorf("computing links: %w", err)
			}
			if err := mesh.AssignAddresses(links, cfg); err != nil {
				return fmt.Errorf("assigning addresses: %w", err)
			}
			if err := generate.CheckInterfaceNameCollisions(cfg, links); err != nil {
				return err
			}

			outputDir := cfg.Global.OutputDir

			for _, node := range cfg.Nodes {
				nodeDir := filepath.Join(outputDir, node.Name)
				if err := os.MkdirAll(nodeDir, 0o755); err != nil {
					return fmt.Errorf("creating output dir for %s: %w", node.Name, err)
				}

				peers := generate.BuildWGPeers(cfg, node.Name, links)

				switch node.Type {
				case config.NodeTypeLinux:
					if err := generateLinux(cfg, &node, peers, links, nodeDir); err != nil {
						return fmt.Errorf("generating linux config for %s: %w", node.Name, err)
					}
				case config.NodeTypeRouterOS:
					if err := generateRouterOS(cfg, &node, peers, links, nodeDir); err != nil {
						return fmt.Errorf("generating routeros config for %s: %w", node.Name, err)
					}
				case config.NodeTypeStatic:
					if err := generateStatic(cfg, &node, peers, links, nodeDir); err != nil {
						return fmt.Errorf("generating static config for %s: %w", node.Name, err)
					}
				}

				fmt.Printf("  generated: %s/ (%s)\n", node.Name, node.Type)
			}

			fmt.Printf("output written to %s\n", outputDir)
			return nil
		},
	}
	cmd.Flags().StringVarP(&configPath, "config", "c", "meshctl.yaml", "path to inventory YAML")
	return cmd
}

func generateLinux(cfg *config.Config, node *config.Node, peers []generate.WGPeerConfig, links []mesh.Link, outDir string) error {
	gen, err := generate.NewBIRDGenerator(cfg)
	if err != nil {
		return err
	}

	// wireguard.json
	wgData, err := gen.GenerateWireguard(node, peers)
	if err != nil {
		return fmt.Errorf("wireguard: %w", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "wireguard.json"), wgData, 0o644); err != nil {
		return err
	}

	// bird-meshctl.conf
	birdData, err := gen.GenerateOSPF(node, links)
	if err != nil {
		return fmt.Errorf("bird: %w", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "bird-meshctl.conf"), birdData, 0o644); err != nil {
		return err
	}

	return nil
}

func generateRouterOS(cfg *config.Config, node *config.Node, peers []generate.WGPeerConfig, links []mesh.Link, outDir string) error {
	gen := generate.NewRouterOSGenerator(cfg)

	wgData, err := gen.GenerateWireguard(node, peers)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, "wireguard.rsc"), wgData, 0o644); err != nil {
		return err
	}

	ospfData, err := gen.GenerateOSPF(node, links)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, "ospf.rsc"), ospfData, 0o644); err != nil {
		return err
	}

	fullData, err := gen.GenerateFull(node, peers, links)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, "full-setup.rsc"), fullData, 0o644); err != nil {
		return err
	}

	return nil
}

func generateStatic(cfg *config.Config, node *config.Node, peers []generate.WGPeerConfig, links []mesh.Link, outDir string) error {
	gen := generate.NewStaticSnippetGenerator(cfg)

	fullData, err := gen.GenerateFull(node, peers, links)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, "wireguard.conf.snippet"), fullData, 0o644); err != nil {
		return err
	}

	return nil
}

func validateCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate inventory YAML",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			// Also validate that all links are viable (e.g. at least one
			// side has a public endpoint).
			links, err := mesh.ComputeLinks(cfg)
			if err != nil {
				return fmt.Errorf("link validation: %w", err)
			}
			if err := generate.CheckInterfaceNameCollisions(cfg, links); err != nil {
				return err
			}
			fmt.Println("configuration is valid")
			return nil
		},
	}
	cmd.Flags().StringVarP(&configPath, "config", "c", "meshctl.yaml", "path to inventory YAML")
	return cmd
}

func diffCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "diff",
		Short: "Show diff between current and newly generated configs",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}

			// Generate to a temp directory and diff against existing output.
			tmpDir, err := os.MkdirTemp("", "meshctl-diff-*")
			if err != nil {
				return err
			}
			defer os.RemoveAll(tmpDir)

			// Temporarily override output dir.
			origOutput := cfg.Global.OutputDir
			cfg.Global.OutputDir = tmpDir

			links, err := mesh.ComputeLinks(cfg)
			if err != nil {
				return fmt.Errorf("computing links: %w", err)
			}
			if err := mesh.AssignAddresses(links, cfg); err != nil {
				return err
			}

			for _, node := range cfg.Nodes {
				nodeDir := filepath.Join(tmpDir, node.Name)
				if err := os.MkdirAll(nodeDir, 0o755); err != nil {
					return fmt.Errorf("creating temp dir for %s: %w", node.Name, err)
				}
				peers := generate.BuildWGPeers(cfg, node.Name, links)

				switch node.Type {
				case config.NodeTypeLinux:
					if err := generateLinux(cfg, &node, peers, links, nodeDir); err != nil {
						return fmt.Errorf("generating linux config for %s: %w", node.Name, err)
					}
				case config.NodeTypeRouterOS:
					if err := generateRouterOS(cfg, &node, peers, links, nodeDir); err != nil {
						return fmt.Errorf("generating routeros config for %s: %w", node.Name, err)
					}
				case config.NodeTypeStatic:
					if err := generateStatic(cfg, &node, peers, links, nodeDir); err != nil {
						return fmt.Errorf("generating static config for %s: %w", node.Name, err)
					}
				}
			}

			// Walk new files: detect NEW and CHANGED.
			hasDiff := false
			if err := filepath.Walk(tmpDir, func(path string, info os.FileInfo, err error) error {
				if err != nil || info.IsDir() {
					return nil
				}
				rel, _ := filepath.Rel(tmpDir, path)
				origPath := filepath.Join(origOutput, rel)

				newData, err := os.ReadFile(path)
				if err != nil {
					return fmt.Errorf("reading generated file %s: %w", rel, err)
				}
				oldData, readErr := os.ReadFile(origPath)

				if readErr != nil {
					fmt.Printf("+ NEW: %s\n", rel)
					hasDiff = true
				} else if string(oldData) != string(newData) {
					fmt.Printf("~ CHANGED: %s\n", rel)
					hasDiff = true
				}
				return nil
			}); err != nil {
				return err
			}

			// Walk existing output: detect DELETED files/dirs.
			if _, err := os.Stat(origOutput); err == nil {
				newNodes := make(map[string]bool)
				for _, node := range cfg.Nodes {
					newNodes[node.Name] = true
				}
				filepath.Walk(origOutput, func(path string, info os.FileInfo, err error) error {
					if err != nil {
						return nil
					}
					rel, _ := filepath.Rel(origOutput, path)
					if rel == "." {
						return nil
					}
					// Check if it's a top-level node dir that no longer exists.
					if info.IsDir() {
						if !newNodes[rel] && filepath.Dir(rel) == "." {
							fmt.Printf("- DELETED: %s/\n", rel)
							hasDiff = true
							return filepath.SkipDir
						}
						return nil
					}
					// Check if file still exists in new output.
					tmpPath := filepath.Join(tmpDir, rel)
					if _, err := os.Stat(tmpPath); os.IsNotExist(err) {
						fmt.Printf("- DELETED: %s\n", rel)
						hasDiff = true
					}
					return nil
				})
			}

			if !hasDiff {
				fmt.Println("no changes")
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&configPath, "config", "c", "meshctl.yaml", "path to inventory YAML")
	return cmd
}

// pskCmd derives and prints the preshared key for a link. Useful for
// operators configuring RouterOS peers by hand, since RouterOS cannot
// run the agent and therefore cannot derive keys itself.
func pskCmd() *cobra.Command {
	var masterFile string
	cmd := &cobra.Command{
		Use:   "psk <node-a> <node-b>",
		Short: "Derive the PSK for a mesh link",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			master, err := psk.LoadMaster(masterFile)
			if err != nil {
				return err
			}
			fmt.Println(psk.DeriveBase64(master, args[0], args[1]))
			return nil
		},
	}
	cmd.Flags().StringVarP(&masterFile, "master", "m",
		"/etc/meshctl/psk-master.key",
		"path to shared PSK master secret")
	return cmd
}

func showMeshCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "show-mesh",
		Short: "Display mesh topology summary",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}

			links, err := mesh.ComputeLinks(cfg)
			if err != nil {
				return fmt.Errorf("computing links: %w", err)
			}
			if err := mesh.AssignAddresses(links, cfg); err != nil {
				return err
			}

			fmt.Printf("Mesh: %d nodes, %d links\n\n", len(cfg.Nodes), len(links))

			fmt.Println("Nodes:")
			for _, n := range cfg.Nodes {
				peerCount := len(mesh.LinksForNode(links, n.Name))
				fmt.Printf("  %-20s %-10s loopback=%-16s peers=%d\n",
					n.Name, n.Type, n.Loopback, peerCount)
			}

			fmt.Println("\nLinks:")
			for _, l := range links {
				mode := "fe80"
				addr := ""
				if l.Mode == mesh.LinkModeV4LL {
					mode = "v4ll"
					addr = fmt.Sprintf(" %s <-> %s", l.AddrA, l.AddrB)
				}
				fmt.Printf("  %-15s <-> %-15s  [%s]%s\n",
					l.NodeA, l.NodeB, mode, addr)
			}

			// Show per-node peer summary with ports.
			fmt.Println("\nPer-node interfaces:")
			for _, n := range cfg.Nodes {
				nodeLinks := mesh.LinksForNode(links, n.Name)
				var ifaces []string
				for _, l := range nodeLinks {
					peer := l.PeerName(n.Name)
					iface := generate.WGInterfaceName(cfg.Global.WGIfacePrefix, peer)
					port := generate.PeerPort(cfg, n.Name, peer, links)
					ifaces = append(ifaces, fmt.Sprintf("%s:%d", iface, port))
				}
				fmt.Printf("  %s: %s\n", n.Name, strings.Join(ifaces, ", "))
			}

			return nil
		},
	}
	cmd.Flags().StringVarP(&configPath, "config", "c", "meshctl.yaml", "path to inventory YAML")
	return cmd
}
