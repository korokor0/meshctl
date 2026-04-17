package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/honoka/meshctl/internal/agent"
)

func main() {
	var configPath string
	var debug bool

	root := &cobra.Command{
		Use:   "meshctl-agent",
		Short: "Mesh network node agent — config sync, probe, cost adjust",
		RunE: func(cmd *cobra.Command, args []string) error {
			level := slog.LevelInfo
			if debug {
				level = slog.LevelDebug
			}
			logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
				Level: level,
			}))

			cfg, err := agent.LoadAgentConfig(configPath)
			if err != nil {
				return err
			}

			logger.Info("starting meshctl-agent",
				"node", cfg.NodeName,
				"probe_port", cfg.ProbePort,
				"config_sync_interval", cfg.ConfigSyncInterval,
				"probe_interval", cfg.ProbeInterval)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// Graceful shutdown on SIGINT/SIGTERM.
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
			go func() {
				<-sig
				logger.Info("received shutdown signal")
				cancel()
			}()

			a := agent.New(cfg, logger, debug)
			return a.Run(ctx)
		},
	}

	root.PersistentFlags().StringVarP(&configPath, "config", "c", "/etc/meshctl/agent.yaml", "path to agent config")
	root.Flags().BoolVar(&debug, "debug", false, "enable debug logging (includes SSH verbose output)")

	// --- status subcommand ---
	var statusAddr string
	var statusJSON bool

	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Show agent status — peer latency, OSPF cost, and health",
		Long: `Reads agent status from local files (default) or HTTP endpoint.

By default, reads /var/run/meshctl/health.json and /var/run/meshctl/peers.json
written by the running agent. Use --addr to query the HTTP endpoint instead.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if statusAddr != "" {
				// HTTP mode.
				addr := statusAddr
				if strings.HasPrefix(addr, ":") {
					addr = "127.0.0.1" + addr
				}
				if !strings.HasPrefix(addr, "http") {
					addr = "http://" + addr
				}
				return runStatusHTTP(addr, statusJSON)
			}

			// File mode (default): read from health + peers JSON files.
			cfg, err := agent.LoadAgentConfig(configPath)
			if err != nil {
				return fmt.Errorf("cannot load agent config: %w", err)
			}
			return runStatusFile(cfg.HealthFile, statusJSON)
		},
	}
	statusCmd.Flags().StringVar(&statusAddr, "addr", "", "query agent HTTP endpoint instead of local files (e.g. :9474)")
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "output raw JSON")

	root.AddCommand(statusCmd)

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

// runStatusFile reads health and peer status from local JSON files.
func runStatusFile(healthFile string, jsonOutput bool) error {
	healthData, err := os.ReadFile(healthFile)
	if err != nil {
		return fmt.Errorf("cannot read health file %s — is meshctl-agent running?\n  %w", healthFile, err)
	}

	var health agent.HealthStatus
	if err := json.Unmarshal(healthData, &health); err != nil {
		return fmt.Errorf("parsing health file: %w", err)
	}

	// Peers file is alongside the health file.
	peersFile := peersFileFromHealth(healthFile)
	var peers []peerStatusDisplay
	if peersData, err := os.ReadFile(peersFile); err == nil {
		json.Unmarshal(peersData, &peers)
	}

	if jsonOutput {
		return printJSON(health, peers)
	}

	printHealth(health)
	if len(peers) > 0 {
		fmt.Println()
		printPeers(peers)
	}
	return nil
}

// runStatusHTTP queries the agent's HTTP health endpoint.
func runStatusHTTP(baseURL string, jsonOutput bool) error {
	client := &http.Client{Timeout: 5 * time.Second}

	type result struct {
		data []byte
		err  error
	}
	healthCh := make(chan result, 1)
	peersCh := make(chan result, 1)

	go func() {
		data, err := httpGet(client, baseURL+"/health")
		healthCh <- result{data, err}
	}()
	go func() {
		data, err := httpGet(client, baseURL+"/peers")
		peersCh <- result{data, err}
	}()

	healthRes := <-healthCh
	peersRes := <-peersCh

	if healthRes.err != nil {
		return fmt.Errorf("cannot reach agent at %s/health: %w", baseURL, healthRes.err)
	}

	var health agent.HealthStatus
	if err := json.Unmarshal(healthRes.data, &health); err != nil {
		return fmt.Errorf("parsing health response: %w", err)
	}

	var peers []peerStatusDisplay
	if peersRes.err == nil {
		json.Unmarshal(peersRes.data, &peers)
	}

	if jsonOutput {
		return printJSON(health, peers)
	}

	printHealth(health)
	if len(peers) > 0 {
		fmt.Println()
		printPeers(peers)
	}
	return nil
}

type peerStatusDisplay struct {
	Name             string `json:"name"`
	Interface        string `json:"interface"`
	Type             string `json:"type"`
	ForwardDelay     string `json:"forward_delay"`
	RTT              string `json:"rtt"`
	Band             int    `json:"band"`
	Cost             uint32 `json:"cost"`
	BandwidthPenalty uint32 `json:"bandwidth_penalty"`
	Mode             string `json:"mode"`
	Status           string `json:"status"`
}

func printJSON(health agent.HealthStatus, peers []peerStatusDisplay) error {
	out := struct {
		Health agent.HealthStatus  `json:"health"`
		Peers  []peerStatusDisplay `json:"peers"`
	}{health, peers}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func printHealth(h agent.HealthStatus) {
	fmt.Printf("Node:        %s\n", h.NodeName)
	fmt.Printf("Uptime:      %s\n", time.Since(h.StartedAt).Truncate(time.Second))
	fmt.Printf("Config age:  %s\n", h.ConfigAge)
	if h.LastFetchError != "" {
		fmt.Printf("Fetch error: %s\n", h.LastFetchError)
	}
	if !h.LastProbeRound.IsZero() {
		fmt.Printf("Last probe:  %s ago\n", time.Since(h.LastProbeRound).Truncate(time.Second))
	}
	fmt.Printf("Peers:       %d\n", h.PeerCount)
	if h.NTPSynced != nil {
		syncStr := "yes"
		if !*h.NTPSynced {
			syncStr = "NO"
		}
		fmt.Printf("NTP synced:  %s", syncStr)
		if h.NTPOffset != "" {
			fmt.Printf("  (offset %s)", h.NTPOffset)
		}
		fmt.Println()
	}
}

func printPeers(peers []peerStatusDisplay) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "PEER\tINTERFACE\tFORWARD\tRTT\tBAND\tCOST\tMODE\tSTATUS\n")
	for _, p := range peers {
		fwd := p.ForwardDelay
		rtt := p.RTT
		bandStr := fmt.Sprintf("%d", p.Band)
		costStr := fmt.Sprintf("%d", p.Cost)

		if p.BandwidthPenalty > 0 {
			costStr = fmt.Sprintf("%d (+bw%d)", p.Cost, p.BandwidthPenalty)
		}

		if p.Mode == "static" {
			fwd = "-"
			rtt = "-"
			bandStr = "-"
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			p.Name, p.Interface, fwd, rtt, bandStr, costStr, p.Mode, p.Status)
	}
	w.Flush()
}

func httpGet(client *http.Client, url string) ([]byte, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// peersFileFromHealth derives the peers JSON path from the health file path.
// e.g. /var/run/meshctl/health.json → /var/run/meshctl/peers.json
func peersFileFromHealth(healthPath string) string {
	dir := healthPath
	for i := len(dir) - 1; i >= 0; i-- {
		if dir[i] == '/' {
			dir = dir[:i]
			break
		}
	}
	return dir + "/peers.json"
}
