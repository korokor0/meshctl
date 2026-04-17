package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/honoka/meshctl/internal/cost"
	"github.com/honoka/meshctl/internal/probe"
)

// Agent is the main meshctl-agent runtime.
type Agent struct {
	cfg        *AgentConfig
	fetcher    *Fetcher
	applier    *Applier
	probeServer *probe.Server
	probeClient *probe.Client
	costEngine *cost.Engine
	health     *HealthTracker
	logger     *slog.Logger
	debug      bool

	// peerAddrs maps peer name → tunnel IP for probing.
	// Updated on each config sync.
	mu            sync.RWMutex
	peerAddrs     map[string]string
	peerTypes     map[string]string // "linux", "routeros", "static"
	peerIfaces    map[string]string // peer name → WG interface name
	lastConfigDir string            // last fetched config directory
}

// New creates a new agent from the given config.
func New(cfg *AgentConfig, logger *slog.Logger, debug bool) *Agent {
	bands := []cost.Band{
		{UpThreshold: 0, DownThreshold: 0, Cost: 20, HoldCount: 5},                                            // <4ms
		{UpThreshold: 4 * time.Millisecond, DownThreshold: 2 * time.Millisecond, Cost: 80, HoldCount: 5},       // 4-12ms    (+60)
		{UpThreshold: 12 * time.Millisecond, DownThreshold: 8 * time.Millisecond, Cost: 160, HoldCount: 5},     // 12-30ms   (+80)
		{UpThreshold: 30 * time.Millisecond, DownThreshold: 20 * time.Millisecond, Cost: 250, HoldCount: 5},    // 30-60ms   (+90)
		{UpThreshold: 60 * time.Millisecond, DownThreshold: 40 * time.Millisecond, Cost: 350, HoldCount: 5},    // 60-100ms  (+100)
		{UpThreshold: 100 * time.Millisecond, DownThreshold: 70 * time.Millisecond, Cost: 480, HoldCount: 5},   // 100-160ms (+130)
		{UpThreshold: 160 * time.Millisecond, DownThreshold: 120 * time.Millisecond, Cost: 640, HoldCount: 5},  // 160-220ms (+160)
		{UpThreshold: 220 * time.Millisecond, DownThreshold: 180 * time.Millisecond, Cost: 840, HoldCount: 5},  // 220-300ms (+200)
		{UpThreshold: 300 * time.Millisecond, DownThreshold: 260 * time.Millisecond, Cost: 1100, HoldCount: 5}, // 300ms+    (+260)
	}

	return &Agent{
		cfg:         cfg,
		debug:       debug,
		fetcher:     NewFetcher(cfg, logger, debug),
		applier:     NewApplier(cfg.BIRDSocket, cfg.BIRDIncludePath, cfg.PrivateKeyFile, cfg.PSKMasterFile, cfg.NodeName, logger),
		probeServer: probe.NewServer(fmt.Sprintf(":%d", cfg.ProbePort), logger),
		probeClient: probe.NewClient(cfg.ProbePort, 5*time.Second, logger),
		costEngine:  cost.NewEngine(bands, 65535, 0.3, 3),
		health:      NewHealthTracker(cfg.NodeName, cfg.HealthFile, logger),
		logger:      logger,
		peerAddrs:   make(map[string]string),
		peerTypes:   make(map[string]string),
		peerIfaces:  make(map[string]string),
	}
}

// Run starts the agent's three independent loops and blocks until ctx is cancelled.
// On context cancellation, it performs a graceful shutdown: stops probing and
// config sync first, then stops the probe server and health endpoint, and
// writes a final health status before returning.
func (a *Agent) Run(ctx context.Context) error {
	// Check BIRD socket at startup.
	a.checkBIRDSocket()

	// Check NTP clock synchronization at startup.
	a.checkClockSync()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Handle SIGHUP for manual config reload.
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	defer signal.Stop(sighup)

	var wg sync.WaitGroup

	// Health HTTP endpoint (optional).
	if a.cfg.HealthAddr != "" {
		a.health.SetPeersFunc(a.peerStatuses)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := a.health.ServeHTTP(ctx, a.cfg.HealthAddr); err != nil {
				a.logger.Error("health HTTP server error", "error", err)
			}
		}()
	}

	// Loop 1: Probe server (responds to incoming probes).
	wg.Add(1)
	go func() {
		defer wg.Done()
		a.logger.Info("starting probe server", "port", a.cfg.ProbePort)
		if err := a.probeServer.ListenAndServe(ctx); err != nil {
			a.logger.Error("probe server error", "error", err)
		}
	}()

	// Loop 2: Config sync.
	wg.Add(1)
	go func() {
		defer wg.Done()
		a.configSyncLoop(ctx, sighup)
	}()

	// Loop 3: Probe + cost adjustment.
	wg.Add(1)
	go func() {
		defer wg.Done()
		a.probeLoop(ctx)
	}()

	// Wait for all goroutines to finish.
	wg.Wait()

	// Graceful shutdown complete — write final health status.
	a.logger.Info("all loops stopped, shutting down")
	a.health.WriteShutdown()

	return nil
}

// configSyncLoop periodically fetches and applies config.
func (a *Agent) configSyncLoop(ctx context.Context, sighup <-chan os.Signal) {
	// Initial sync.
	a.doConfigSync(ctx)

	ticker := time.NewTicker(a.cfg.ConfigSyncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.doConfigSync(ctx)
		case <-sighup:
			a.logger.Info("SIGHUP received, triggering config sync")
			a.doConfigSync(ctx)
		}
	}
}

// doConfigSync fetches and applies config once.
func (a *Agent) doConfigSync(ctx context.Context) {
	a.logger.Info("starting config sync")

	configDir, err := a.fetcher.Fetch(ctx)
	if err != nil {
		a.logger.Error("config fetch failed", "error", err)
		a.health.RecordFetchError(err)
		return
	}

	if err := a.applier.Apply(configDir); err != nil {
		a.logger.Error("config apply failed", "error", err)
		a.health.RecordFetchError(err)
		return
	}

	a.health.RecordFetchSuccess()

	// Update peer addresses from the fetched wireguard.json.
	a.updatePeerAddrs(configDir)

	a.mu.Lock()
	a.lastConfigDir = configDir
	a.mu.Unlock()

	a.logger.Info("config sync completed")
}

// updatePeerAddrs reads wireguard.json and extracts peer tunnel addresses
// for probing.
func (a *Agent) updatePeerAddrs(configDir string) {
	data, err := os.ReadFile(configDir + "/wireguard.json")
	if err != nil {
		return
	}

	var wgCfg WireguardConfig
	if err := parseJSON(data, &wgCfg); err != nil {
		a.logger.Warn("parsing wireguard.json for peer addrs", "error", err)
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	for _, peer := range wgCfg.Peers {
		// Probe target selection: use the peer's fe80 address scoped to
		// the WireGuard interface. This guarantees the probe traverses the
		// specific WG tunnel, not a routed path via another interface.
		// Fallback to V4LL PeerAddress (has a connected /32 PTP route).
		if peer.PeerFe80 != "" && peer.Interface != "" {
			a.peerAddrs[peer.Name] = peer.PeerFe80 + "%" + peer.Interface
		} else if peer.PeerAddress != "" {
			a.peerAddrs[peer.Name] = peer.PeerAddress
		} else {
			a.logger.Warn("no probe target for peer — missing peer_fe80 and peer_address",
				"peer", peer.Name)
		}

		// Track peer type and interface name.
		if peer.PeerType != "" {
			a.peerTypes[peer.Name] = peer.PeerType
		}
		if peer.Interface != "" {
			a.peerIfaces[peer.Name] = peer.Interface
		}

		// Configure cost engine based on cost_mode.
		if peer.CostMode == "static" && peer.StaticCost != nil {
			a.costEngine.SetStaticCost(peer.Name, *peer.StaticCost)
			a.logger.Info("peer uses static cost",
				"peer", peer.Name, "cost", *peer.StaticCost)
		} else if peer.StaticCost != nil {
			// Probe mode with static_cost: use as fallback on failure.
			a.costEngine.SetFallbackCost(peer.Name, *peer.StaticCost)
			a.logger.Info("peer uses probe with static fallback",
				"peer", peer.Name, "fallback_cost", *peer.StaticCost)
		}

		// Apply bandwidth penalty (additive cost for low-bandwidth links).
		if peer.BandwidthPenalty > 0 {
			a.costEngine.SetBandwidthPenalty(peer.Name, peer.BandwidthPenalty)
			a.logger.Info("peer bandwidth penalty",
				"peer", peer.Name, "penalty", peer.BandwidthPenalty)
		}
	}

	// Reload cost bands from config if present.
	if len(wgCfg.CostBands) > 0 {
		bands := make([]cost.Band, 0, len(wgCfg.CostBands))
		for _, b := range wgCfg.CostBands {
			bands = append(bands, cost.Band{
				UpThreshold:   time.Duration(b.UpMs) * time.Millisecond,
				DownThreshold: time.Duration(b.DownMs) * time.Millisecond,
				Cost:          b.Cost,
				HoldCount:     b.Hold,
			})
		}
		penaltyCost := wgCfg.PenaltyCost
		if penaltyCost == 0 {
			penaltyCost = 65535
		}
		alpha := wgCfg.EWMAAlpha
		if alpha == 0 {
			alpha = 0.3
		}
		failThreshold := wgCfg.FailThreshold
		if failThreshold == 0 {
			failThreshold = 3
		}
		a.costEngine.UpdateConfig(bands, penaltyCost, alpha, failThreshold)
		a.logger.Info("cost bands reloaded from config", "bands", len(bands))
	}
}

// probeLoop periodically probes all peers and adjusts OSPF costs.
func (a *Agent) probeLoop(ctx context.Context) {
	// Wait for initial config sync.
	time.Sleep(5 * time.Second)

	ticker := time.NewTicker(a.cfg.ProbeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.doProbeRound(ctx)
		}
	}
}

// doProbeRound probes all peers and updates costs.
func (a *Agent) doProbeRound(ctx context.Context) {
	a.mu.RLock()
	addrs := make(map[string]string, len(a.peerAddrs))
	for k, v := range a.peerAddrs {
		// Skip peers with static cost — no probing needed.
		if a.costEngine.IsStatic(k) {
			continue
		}
		addrs[k] = v
	}
	a.mu.RUnlock()

	if len(addrs) == 0 {
		return
	}

	results := a.probeClient.ProbeAll(ctx, addrs)

	anyChanged := false
	for _, r := range results {
		if !r.Valid {
			if a.costEngine.RecordFailure(r.Peer) {
				anyChanged = true
				a.logger.Warn("peer unreachable", "peer", r.Peer,
					"cost", a.costEngine.CurrentCost(r.Peer))
			}
			continue
		}

		changed := a.costEngine.RecordSuccess(r.Peer, r.ForwardDelay, r.RTT)
		if changed {
			anyChanged = true
			a.logger.Info("cost band changed", "peer", r.Peer,
				"forward_delay", r.ForwardDelay,
				"cost", a.costEngine.CurrentCost(r.Peer))
		}
	}

	if anyChanged {
		if err := a.applyDynamicCosts(); err != nil {
			a.logger.Error("failed to apply dynamic costs to BIRD", "error", err)
		}
	}

	a.health.RecordProbeRound(len(addrs))

	// Write per-peer status file for `meshctl-agent status` CLI.
	a.health.WritePeers(a.peerStatuses())
}

// checkBIRDSocket verifies that the BIRD control socket exists at startup.
// If the socket is missing, the agent can still run (probing works), but
// cost updates will fail silently on every cycle.
func (a *Agent) checkBIRDSocket() {
	if _, err := os.Stat(a.cfg.BIRDSocket); err != nil {
		a.logger.Warn("BIRD control socket not found — OSPF cost updates will fail until BIRD is running",
			"path", a.cfg.BIRDSocket, "error", err)
	} else {
		a.logger.Info("BIRD control socket OK", "path", a.cfg.BIRDSocket)
	}
}

// checkClockSync verifies NTP synchronization by running chronyc/timedatectl
// and warns if the clock offset exceeds 10ms. One-way delay measurement
// depends on synchronized clocks; large offsets produce incorrect OSPF costs.
func (a *Agent) checkClockSync() {
	offset, synced, err := checkNTPOffset()
	if err != nil {
		a.logger.Warn("could not check NTP sync — one-way delay measurements may be inaccurate",
			"error", err)
		return
	}
	if !synced {
		a.logger.Warn("NTP is not synchronized — one-way delay measurements will be inaccurate; install chrony or ntpd")
		a.health.RecordNTPStatus(false, 0)
		return
	}
	a.health.RecordNTPStatus(true, offset)
	if offset > 10*time.Millisecond || offset < -10*time.Millisecond {
		a.logger.Warn("NTP clock offset exceeds 10ms — one-way delay measurements may be inaccurate",
			"offset", offset)
	} else {
		a.logger.Info("NTP clock sync OK", "offset", offset)
	}
}

// checkNTPOffset tries chronyc first, then timedatectl, to determine the
// current clock offset and sync status.
func checkNTPOffset() (offset time.Duration, synced bool, err error) {
	// Try chronyc tracking.
	out, err := exec.Command("chronyc", "tracking").Output()
	if err == nil {
		return parseChronyTracking(string(out))
	}

	// Fallback: timedatectl.
	out, err = exec.Command("timedatectl", "show", "--property=NTPSynchronized").Output()
	if err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "NTPSynchronized=yes") {
				return 0, true, nil
			}
			if strings.HasPrefix(line, "NTPSynchronized=no") {
				return 0, false, nil
			}
		}
	}

	return 0, false, fmt.Errorf("neither chronyc nor timedatectl available")
}

// parseChronyTracking parses chronyc tracking output and extracts offset.
func parseChronyTracking(output string) (time.Duration, bool, error) {
	synced := false
	var offset time.Duration

	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		// "Leap status     : Normal" indicates sync
		if strings.HasPrefix(trimmed, "Leap status") && strings.Contains(trimmed, "Normal") {
			synced = true
		}
		// "Last offset     : +0.000123456 seconds"
		if strings.HasPrefix(trimmed, "Last offset") {
			parts := strings.Fields(trimmed)
			for i, p := range parts {
				if p == ":" && i+1 < len(parts) {
					val := parts[i+1]
					secs := parseFloat(val)
					offset = time.Duration(secs * float64(time.Second))
					break
				}
			}
		}
	}

	if !synced && offset == 0 {
		return 0, false, nil
	}
	return offset, synced, nil
}

// parseFloat parses a float string. Returns 0 on error.
func parseFloat(s string) float64 {
	negative := false
	if len(s) > 0 && s[0] == '-' {
		negative = true
		s = s[1:]
	} else if len(s) > 0 && s[0] == '+' {
		s = s[1:]
	}

	whole := 0.0
	frac := 0.0
	fracDiv := 1.0
	inFrac := false
	for _, c := range s {
		if c == '.' {
			inFrac = true
			continue
		}
		if c < '0' || c > '9' {
			break
		}
		if inFrac {
			fracDiv *= 10
			frac += float64(c-'0') / fracDiv
		} else {
			whole = whole*10 + float64(c-'0')
		}
	}

	result := whole + frac
	if negative {
		result = -result
	}
	return result
}

// applyDynamicCosts rewrites the BIRD include file with updated OSPF costs
// from the cost engine and triggers a BIRD reconfiguration.
func (a *Agent) applyDynamicCosts() error {
	a.mu.RLock()
	ifaces := make(map[string]string, len(a.peerIfaces))
	for k, v := range a.peerIfaces {
		ifaces[k] = v
	}
	a.mu.RUnlock()

	// Build interface → cost map from the cost engine.
	costMap := make(map[string]uint32)
	for peer, iface := range ifaces {
		costMap[iface] = a.costEngine.CurrentCost(peer)
	}

	if len(costMap) == 0 {
		return nil
	}

	// Read the current BIRD include file.
	data, err := os.ReadFile(a.applier.birdInclude)
	if err != nil {
		return fmt.Errorf("reading BIRD include: %w", err)
	}

	// Replace cost values in the BIRD config.
	// Pattern: interface "igp-xxx" { ... cost NNN; ... }
	updated := replaceBIRDCosts(string(data), costMap)

	if updated == string(data) {
		a.logger.Debug("BIRD config unchanged after cost update")
		return nil
	}

	if err := atomicWriteFile(a.applier.birdInclude, []byte(updated), 0o644); err != nil {
		return fmt.Errorf("writing BIRD include: %w", err)
	}

	a.logger.Info("BIRD config updated with new costs, reconfiguring")
	if err := a.applier.birdClient.Configure(); err != nil {
		return fmt.Errorf("birdc configure: %w", err)
	}
	return nil
}

// replaceBIRDCosts replaces OSPF cost values in a BIRD config string.
// It looks for patterns like:
//
//	interface "igp-xxx" {
//	    ...
//	    cost NNN;
//
// and replaces NNN with the value from costMap[ifaceName].
func replaceBIRDCosts(config string, costMap map[string]uint32) string {
	lines := strings.Split(config, "\n")
	var result []string
	currentIface := ""

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Track which interface block we're in.
		if strings.HasPrefix(trimmed, "interface \"") {
			// Extract interface name from: interface "igp-xxx" {
			start := strings.IndexByte(trimmed, '"') + 1
			end := strings.LastIndexByte(trimmed, '"')
			if start > 0 && end > start {
				currentIface = trimmed[start:end]
			}
		}

		// Replace cost line if we're inside a known interface block.
		if currentIface != "" && strings.HasPrefix(trimmed, "cost ") && strings.HasSuffix(trimmed, ";") {
			if newCost, ok := costMap[currentIface]; ok {
				// Preserve indentation.
				indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
				line = indent + fmt.Sprintf("cost %d;", newCost)
			}
		}

		// Detect end of interface block.
		if trimmed == "};" && currentIface != "" {
			currentIface = ""
		}

		result = append(result, line)
	}

	return strings.Join(result, "\n")
}

// peerStatuses returns the current per-peer status for the /peers endpoint.
func (a *Agent) peerStatuses() []PeerStatus {
	a.mu.RLock()
	peerTypes := make(map[string]string, len(a.peerTypes))
	peerIfaces := make(map[string]string, len(a.peerIfaces))
	for k, v := range a.peerTypes {
		peerTypes[k] = v
	}
	for k, v := range a.peerIfaces {
		peerIfaces[k] = v
	}
	a.mu.RUnlock()

	snapshot := a.costEngine.Snapshot()

	var peers []PeerStatus
	for name, s := range snapshot {
		mode := "probe"
		if s.IsStatic {
			mode = "static"
		} else if peerTypes[name] != "linux" {
			mode = "icmp"
		}

		status := "ok"
		if s.Failures > 0 {
			status = fmt.Sprintf("fail(%d)", s.Failures)
		}
		if s.IsStatic {
			status = "-"
		}

		peers = append(peers, PeerStatus{
			Name:             name,
			Interface:        peerIfaces[name],
			Type:             peerTypes[name],
			ForwardDelay:     s.ForwardDelay,
			RTT:              s.RTT,
			Band:             s.CurrentBand,
			Cost:             s.Cost,
			BandwidthPenalty: s.BandwidthPenalty,
			Mode:             mode,
			Status:           status,
		})
	}

	// Sort by name for stable output.
	sort.Slice(peers, func(i, j int) bool {
		return peers[i].Name < peers[j].Name
	})
	return peers
}

// PeerStatus represents a single peer's runtime state for display.
type PeerStatus struct {
	Name             string        `json:"name"`
	Interface        string        `json:"interface,omitempty"`
	Type             string        `json:"type,omitempty"`
	ForwardDelay     time.Duration `json:"forward_delay_ns"`
	RTT              time.Duration `json:"rtt_ns"`
	Band             int           `json:"band"`
	Cost             uint32        `json:"cost"`
	BandwidthPenalty uint32        `json:"bandwidth_penalty,omitempty"`
	Mode             string        `json:"mode"`
	Status           string        `json:"status"`
}

// MarshalJSON provides human-readable durations in JSON output.
func (p PeerStatus) MarshalJSON() ([]byte, error) {
	type Alias PeerStatus
	return json.Marshal(&struct {
		Alias
		ForwardDelayStr string `json:"forward_delay"`
		RTTStr          string `json:"rtt"`
	}{
		Alias:           Alias(p),
		ForwardDelayStr: formatDuration(p.ForwardDelay),
		RTTStr:          formatDuration(p.RTT),
	})
}

func formatDuration(d time.Duration) string {
	if d == 0 {
		return "-"
	}
	if d < time.Millisecond {
		return fmt.Sprintf("%.1fµs", float64(d)/float64(time.Microsecond))
	}
	return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
}

// extractHost strips the /prefix from a CIDR notation address.
func extractHost(cidr string) string {
	for i, c := range cidr {
		if c == '/' {
			return cidr[:i]
		}
	}
	return cidr
}

// parseJSON unmarshals JSON data into v.
func parseJSON(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}
