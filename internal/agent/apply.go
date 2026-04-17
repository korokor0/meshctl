package agent

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/honoka/meshctl/internal/bird"
	"github.com/honoka/meshctl/internal/psk"
)

// UnderlayRouteConfig is an underlay static route from wireguard.json.
type UnderlayRouteConfig struct {
	Peer    string `json:"peer"`
	Dest    string `json:"dest"`    // e.g. "2001:db8::1/128" or "1.2.3.4/32"
	Prefsrc string `json:"prefsrc"` // krt_prefsrc value
	Family  string `json:"family"`  // "ipv4" or "ipv6"
}

// CostBandConfig is a cost band from wireguard.json.
type CostBandConfig struct {
	UpMs   int64  `json:"up_ms"`
	DownMs int64  `json:"down_ms"`
	Cost   uint32 `json:"cost"`
	Hold   int    `json:"hold"`
}

// WireguardConfig is the JSON structure read from wireguard.json.
type WireguardConfig struct {
	Node           string                `json:"node"`
	ListenPort     int                   `json:"listen_port"`
	PSKRequired    bool                  `json:"psk_required,omitempty"`
	Peers          []WGPeer              `json:"peers"`
	UnderlayRoutes []UnderlayRouteConfig `json:"underlay_routes,omitempty"`
	IGPTable4      string                `json:"igp_table4,omitempty"`
	IGPTable6      string                `json:"igp_table6,omitempty"`
	CostBands      []CostBandConfig      `json:"cost_bands,omitempty"`
	PenaltyCost    uint32                `json:"penalty_cost,omitempty"`
	EWMAAlpha      float64               `json:"ewma_alpha,omitempty"`
	FailThreshold  int                   `json:"fail_threshold,omitempty"`
}

// WGPeer describes a single WireGuard peer from the generated config.
type WGPeer struct {
	Name               string   `json:"name"`
	PublicKey          string   `json:"public_key"`
	Endpoint           string   `json:"endpoint"`
	AllowedIPs         []string `json:"allowed_ips"`
	PersistentKeepalive int    `json:"persistent_keepalive"`
	ListenPort         int      `json:"listen_port"`
	Interface          string   `json:"interface"`
	Address            string   `json:"address,omitempty"`      // V4LL local addr (e.g. "169.254.0.4")
	PeerAddress        string   `json:"peer_address,omitempty"` // V4LL peer addr (e.g. "169.254.0.2")
	Fe80Address        string   `json:"fe80_address,omitempty"` // fe80 local addr (e.g. "fe80::127:4/64")
	PeerFe80           string   `json:"peer_fe80,omitempty"`    // peer's fe80 addr (e.g. "fe80::127:3")
	PeerType           string   `json:"peer_type,omitempty"`
	CostMode           string   `json:"cost_mode,omitempty"`
	StaticCost         *uint32  `json:"static_cost,omitempty"`
	BandwidthPenalty   uint32   `json:"bandwidth_penalty,omitempty"`
}

// Applier applies fetched configuration to the local system.
type Applier struct {
	birdClient     *bird.Client
	birdInclude    string
	privateKeyFile string
	pskMasterFile  string
	nodeName       string
	logger         *slog.Logger
}

// NewApplier creates a config applier.
func NewApplier(birdSocket, birdInclude, privateKeyFile, pskMasterFile, nodeName string, logger *slog.Logger) *Applier {
	return &Applier{
		birdClient:     bird.NewClient(birdSocket),
		birdInclude:    birdInclude,
		privateKeyFile: privateKeyFile,
		pskMasterFile:  pskMasterFile,
		nodeName:       nodeName,
		logger:         logger,
	}
}

// Apply reads config from configDir and applies WireGuard + BIRD changes.
func (a *Applier) Apply(configDir string) error {
	if err := a.applyWireguard(configDir); err != nil {
		return fmt.Errorf("applying wireguard: %w", err)
	}
	if err := a.applyUnderlayRoutes(configDir); err != nil {
		return fmt.Errorf("applying underlay routes: %w", err)
	}
	if err := a.applyBIRD(configDir); err != nil {
		return fmt.Errorf("applying bird: %w", err)
	}
	return nil
}

// applyWireguard reads wireguard.json and sets up WG interfaces via wg/ip commands.
// In production this would use wgctrl-go netlink; here we shell out for portability.
func (a *Applier) applyWireguard(configDir string) error {
	data, err := os.ReadFile(filepath.Join(configDir, "wireguard.json"))
	if err != nil {
		if os.IsNotExist(err) {
			a.logger.Info("no wireguard.json, skipping WG apply")
			return nil
		}
		return err
	}

	var wgCfg WireguardConfig
	if err := json.Unmarshal(data, &wgCfg); err != nil {
		return fmt.Errorf("parsing wireguard.json: %w", err)
	}

	// Verify the private key file exists and is readable before proceeding.
	if _, err := os.Stat(a.privateKeyFile); err != nil {
		return fmt.Errorf("private key file %s: %w", a.privateKeyFile, err)
	}

	// Load PSK master once per apply round if configured.
	var pskMaster []byte
	if wgCfg.PSKRequired || a.pskMasterFile != "" {
		if a.pskMasterFile == "" {
			a.logger.Warn("wireguard.json requires PSK but no psk_master_file configured")
		} else {
			m, err := psk.LoadMaster(a.pskMasterFile)
			if err != nil {
				a.logger.Warn("failed to load psk master", "error", err)
			} else {
				pskMaster = m
			}
		}
	}

	var failCount int
	for _, peer := range wgCfg.Peers {
		listenPort := peer.ListenPort
		if listenPort == 0 {
			listenPort = wgCfg.ListenPort // fallback for old config format
		}
		if err := a.ensureWGInterface(peer, listenPort, pskMaster); err != nil {
			a.logger.Warn("failed to configure WG interface",
				"interface", peer.Interface, "error", err)
			failCount++
		}
	}
	if failCount > 0 && failCount == len(wgCfg.Peers) {
		return fmt.Errorf("all %d WireGuard peers failed to configure", failCount)
	}
	return nil
}

// ensureWGInterface creates a WireGuard interface if it doesn't exist and
// configures the peer. Uses ip-link and wg commands. If pskMaster is
// non-nil, derives a per-link PSK and applies it to the peer.
func (a *Applier) ensureWGInterface(peer WGPeer, listenPort int, pskMaster []byte) error {
	iface := peer.Interface

	// Check if interface exists.
	created := false
	if err := exec.Command("ip", "link", "show", iface).Run(); err != nil {
		// Create interface.
		a.logger.Info("creating WG interface", "interface", iface)
		if out, err := exec.Command("ip", "link", "add", iface, "type", "wireguard").CombinedOutput(); err != nil {
			return fmt.Errorf("ip link add %s: %w: %s", iface, err, out)
		}
		created = true
	}

	// Apply the local private key on interface creation. `wg set
	// private-key` expects a path to a file containing a base64-encoded
	// key, which is exactly what PrivateKeyFile stores.
	if created {
		if out, err := exec.Command("wg", "set", iface,
			"private-key", a.privateKeyFile).CombinedOutput(); err != nil {
			return fmt.Errorf("wg set private-key: %w: %s", err, out)
		}
	}

	// Set listen port.
	if out, err := exec.Command("wg", "set", iface, "listen-port",
		fmt.Sprintf("%d", listenPort)).CombinedOutput(); err != nil {
		a.logger.Warn("wg set listen-port", "error", err, "output", string(out))
	}

	// Derive and materialize a per-link PSK, if requested. The file is
	// written with mode 0600 in a temp location and removed immediately
	// after `wg set` has read it.
	var pskPath string
	if pskMaster != nil {
		key := psk.DeriveBase64(pskMaster, a.nodeName, peer.Name)
		f, err := os.CreateTemp("", "wg-psk-*")
		if err != nil {
			return fmt.Errorf("creating psk temp: %w", err)
		}
		pskPath = f.Name()
		defer func() {
			os.Remove(pskPath)
		}()
		if err := os.Chmod(pskPath, 0o600); err != nil {
			f.Close()
			return fmt.Errorf("chmod psk temp: %w", err)
		}
		if _, err := f.WriteString(key + "\n"); err != nil {
			f.Close()
			return fmt.Errorf("writing psk temp: %w", err)
		}
		f.Close()
		// Sanity-check the key decodes cleanly.
		if _, err := base64.StdEncoding.DecodeString(key); err != nil {
			return fmt.Errorf("derived psk not valid base64: %w", err)
		}
	}

	// Add/update peer.
	args := []string{"set", iface, "peer", peer.PublicKey}
	if pskPath != "" {
		args = append(args, "preshared-key", pskPath)
	}
	if peer.Endpoint != "" {
		args = append(args, "endpoint", peer.Endpoint)
	}
	if peer.PersistentKeepalive > 0 {
		args = append(args, "persistent-keepalive", fmt.Sprintf("%d", peer.PersistentKeepalive))
	}
	if len(peer.AllowedIPs) > 0 {
		ips := ""
		for i, ip := range peer.AllowedIPs {
			if i > 0 {
				ips += ","
			}
			ips += ip
		}
		args = append(args, "allowed-ips", ips)
	}
	if out, err := exec.Command("wg", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("wg set peer: %w: %s", err, out)
	}

	// Assign fe80 link-local address (always, for both fe80 and V4LL mode).
	if peer.Fe80Address != "" {
		if out, err := exec.Command("ip", "-6", "addr", "replace",
			peer.Fe80Address, "dev", iface).CombinedOutput(); err != nil {
			a.logger.Warn("ip -6 addr replace fe80", "error", err, "output", string(out))
		}
	}

	// Assign V4LL point-to-point address.
	if peer.Address != "" && peer.PeerAddress != "" {
		// PTP format: ip addr replace <local> peer <remote>/32 dev <iface>
		if out, err := exec.Command("ip", "addr", "replace",
			peer.Address, "peer", peer.PeerAddress+"/32",
			"dev", iface).CombinedOutput(); err != nil {
			a.logger.Warn("ip addr replace v4ll", "error", err, "output", string(out))
		}
	} else if peer.Address != "" {
		// Legacy fallback: old format with CIDR (e.g. "169.254.x.x/31")
		if out, err := exec.Command("ip", "addr", "replace", peer.Address,
			"dev", iface).CombinedOutput(); err != nil {
			a.logger.Warn("ip addr replace", "error", err, "output", string(out))
		}
	}

	// Bring interface up.
	if out, err := exec.Command("ip", "link", "set", iface, "up").CombinedOutput(); err != nil {
		return fmt.Errorf("ip link set up: %w: %s", err, out)
	}

	return nil
}

// applyUnderlayRoutes reads underlay route config from wireguard.json,
// detects the default gateway for each route, and writes a BIRD static
// protocol include file. The agent resolves "via" at runtime because
// the default gateway is node-local state not known at generate time.
func (a *Applier) applyUnderlayRoutes(configDir string) error {
	data, err := os.ReadFile(filepath.Join(configDir, "wireguard.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var wgCfg WireguardConfig
	if err := json.Unmarshal(data, &wgCfg); err != nil {
		return fmt.Errorf("parsing wireguard.json: %w", err)
	}

	if len(wgCfg.UnderlayRoutes) == 0 {
		return nil
	}

	// Group routes by family, resolving prefsrc and gateway.
	var routes4, routes6 []resolvedUnderlayRoute
	for _, ur := range wgCfg.UnderlayRoutes {
		// Extract the IP from dest (e.g. "1.2.3.4/32" → "1.2.3.4")
		destIP := ur.Dest
		for i, c := range destIP {
			if c == '/' {
				destIP = destIP[:i]
				break
			}
		}

		// Resolve prefsrc: can be an IP, interface name, or "auto".
		prefsrc := resolvePrefsrc(ur.Prefsrc, ur.Family, destIP)
		if prefsrc == "" {
			a.logger.Warn("could not resolve prefsrc for underlay route",
				"prefsrc", ur.Prefsrc, "dest", ur.Dest, "peer", ur.Peer)
			continue
		}

		// Detect gateway via `ip route get`.
		gw := detectGateway(destIP, ur.Family)
		if gw == "" {
			a.logger.Warn("could not detect gateway for underlay route",
				"dest", ur.Dest, "peer", ur.Peer)
			continue
		}

		r := resolvedUnderlayRoute{
			PeerName: ur.Peer,
			Dest:     ur.Dest,
			Via:      gw,
			Prefsrc:  prefsrc,
		}
		if ur.Family == "ipv4" {
			routes4 = append(routes4, r)
		} else {
			routes6 = append(routes6, r)
		}
	}

	if len(routes4) == 0 && len(routes6) == 0 {
		return nil
	}

	// Generate BIRD static protocol config.
	var buf []byte
	buf = append(buf, "# meshctl underlay static routes — auto-generated, do not edit\n"...)

	if len(routes6) > 0 {
		buf = append(buf, "\nprotocol static meshctl_underlay6 {\n"...)
		buf = append(buf, "    ipv6 {\n        table master6;\n        import all;\n        export none;\n    };\n"...)
		for _, r := range routes6 {
			buf = append(buf, fmt.Sprintf("    # %s\n    route %s via %s {\n        krt_prefsrc = %s;\n    };\n",
				r.PeerName, r.Dest, r.Via, r.Prefsrc)...)
		}
		buf = append(buf, "}\n"...)
	}

	if len(routes4) > 0 {
		buf = append(buf, "\nprotocol static meshctl_underlay4 {\n"...)
		buf = append(buf, "    ipv4 {\n        table master4;\n        import all;\n        export none;\n    };\n"...)
		for _, r := range routes4 {
			buf = append(buf, fmt.Sprintf("    # %s\n    route %s via %s {\n        krt_prefsrc = %s;\n    };\n",
				r.PeerName, r.Dest, r.Via, r.Prefsrc)...)
		}
		buf = append(buf, "}\n"...)
	}

	// Write to a separate include file alongside the main BIRD include.
	underlayPath := a.birdInclude[:len(a.birdInclude)-len(filepath.Ext(a.birdInclude))] + "-underlay.conf"
	existing, _ := os.ReadFile(underlayPath)
	if string(existing) == string(buf) {
		a.logger.Debug("underlay routes unchanged, skipping")
		return nil
	}

	if err := atomicWriteFile(underlayPath, buf, 0o644); err != nil {
		return fmt.Errorf("writing underlay routes: %w", err)
	}
	a.logger.Info("underlay routes updated", "path", underlayPath,
		"v4_routes", len(routes4), "v6_routes", len(routes6))
	return nil
}

type resolvedUnderlayRoute struct {
	PeerName string
	Dest     string
	Via      string
	Prefsrc  string
}

// resolvePrefsrc resolves a prefsrc value that may be a literal IP, an
// interface name, or "auto". Returns an IP string suitable for BIRD
// krt_prefsrc, or "" on failure.
//
//   - Literal IP: returned as-is after validation.
//   - "auto": runs `ip route get` to a well-known destination and parses the
//     kernel-selected source address from the output.
//   - Interface name: runs `ip addr show dev <iface>` and picks the first
//     address whose preferred_lft is not 0 (following the kernel's own
//     source address selection rules). If multiple addresses share the same
//     preferred_lft, the first one listed by `ip addr` is used (which
//     matches the kernel's default behavior of preferring the primary
//     address — the one added first).
func resolvePrefsrc(value, family, destIP string) string {
	// Try parsing as a literal IP first.
	if addr, err := netip.ParseAddr(value); err == nil {
		return addr.String()
	}

	if value == "auto" {
		return resolvePrefsrcAuto(family)
	}

	// Treat as interface name.
	return resolvePrefsrcIface(value, family)
}

// resolvePrefsrcAuto detects the source IP from the default route.
// First checks `ip route show default` for a "src" field. If the default
// route has no explicit src, falls back to `ip route get` to let the
// kernel pick the source address.
func resolvePrefsrcAuto(family string) string {
	flag := "-4"
	if family == "ipv6" {
		flag = "-6"
	}

	// Try the default route first.
	out, err := exec.Command("ip", flag, "route", "show", "default").Output()
	if err == nil {
		fields := strings.Fields(string(out))
		for i, f := range fields {
			if f == "src" && i+1 < len(fields) {
				return fields[i+1]
			}
		}
		// Default route exists but has no src — extract the dev and
		// resolve the primary IP from that interface.
		for i, f := range fields {
			if f == "dev" && i+1 < len(fields) {
				if addr := resolvePrefsrcIface(fields[i+1], family); addr != "" {
					return addr
				}
			}
		}
	}

	// Fallback: ip route get to let the kernel select src.
	var dest string
	if family == "ipv6" {
		dest = "2001:4860:4860::8888"
	} else {
		dest = "1.1.1.1"
	}
	out, err = exec.Command("ip", flag, "route", "get", dest).Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "src" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

// resolvePrefsrcIface extracts the primary IP from a named interface.
// It runs `ip -4/-6 addr show dev <iface>` and returns the first address
// that has a non-zero preferred_lft, following the kernel's source address
// selection order.
func resolvePrefsrcIface(iface, family string) string {
	flag := "-4"
	if family == "ipv6" {
		flag = "-6"
	}
	out, err := exec.Command("ip", flag, "addr", "show", "dev", iface).Output()
	if err != nil {
		return ""
	}
	// Parse "inet[6] <addr>/<prefix> ..." lines.
	// The kernel lists primary (first-added) addresses before secondary ones.
	// We skip deprecated addresses (preferred_lft 0sec) and return the
	// first valid one, which mirrors the kernel's own source selection.
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if fields[0] != "inet" && fields[0] != "inet6" {
			continue
		}
		// Check for deprecated: "preferred_lft 0sec" later in the line.
		deprecated := false
		for j, f := range fields {
			if f == "preferred_lft" && j+1 < len(fields) && fields[j+1] == "0sec" {
				deprecated = true
				break
			}
		}
		if deprecated {
			continue
		}
		// Extract IP from "addr/prefix".
		addrStr := fields[1]
		for k, c := range addrStr {
			if c == '/' {
				addrStr = addrStr[:k]
				break
			}
		}
		if _, err := netip.ParseAddr(addrStr); err == nil {
			return addrStr
		}
	}
	return ""
}

// detectGateway uses `ip route get` to find the next-hop for a destination.
func detectGateway(destIP, family string) string {
	var cmd *exec.Cmd
	if family == "ipv6" {
		cmd = exec.Command("ip", "-6", "route", "get", destIP)
	} else {
		cmd = exec.Command("ip", "-4", "route", "get", destIP)
	}
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	// Parse "... via <gateway> ..." from output.
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "via" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

// applyBIRD copies the BIRD include and triggers reconfiguration.
func (a *Applier) applyBIRD(configDir string) error {
	srcPath := filepath.Join(configDir, "bird-meshctl.conf")
	data, err := os.ReadFile(srcPath)
	if err != nil {
		if os.IsNotExist(err) {
			a.logger.Info("no bird-meshctl.conf, skipping BIRD apply")
			return nil
		}
		return err
	}

	// Check if the file changed.
	existing, _ := os.ReadFile(a.birdInclude)
	if string(existing) == string(data) {
		a.logger.Debug("BIRD config unchanged, skipping reconfigure")
		return nil
	}

	if err := atomicWriteFile(a.birdInclude, data, 0o644); err != nil {
		return fmt.Errorf("writing BIRD include: %w", err)
	}

	a.logger.Info("BIRD config updated, reconfiguring")
	if err := a.birdClient.Configure(); err != nil {
		return fmt.Errorf("birdc configure: %w", err)
	}
	return nil
}

