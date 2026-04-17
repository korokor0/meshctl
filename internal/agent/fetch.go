// Package agent implements the meshctl-agent main loop: config sync,
// latency probing, and dynamic OSPF cost adjustment.
package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// AgentConfig is the agent's own configuration, read from agent.yaml.
type AgentConfig struct {
	NodeName           string        `yaml:"node_name"`
	Repo               RepoConfig    `yaml:"repo"`
	ConfigSyncInterval time.Duration `yaml:"config_sync_interval"`
	ProbeInterval      time.Duration `yaml:"probe_interval"`
	ProbePort          int           `yaml:"probe_port"`
	BIRDSocket         string        `yaml:"bird_socket"`
	BIRDIncludePath    string        `yaml:"bird_include_path"`

	// PrivateKeyFile is the local path to this node's WireGuard private key.
	// The file must contain a single base64-encoded key (as produced by
	// `wg genkey`). It is never included in the config repo and must be
	// created out-of-band per node.
	PrivateKeyFile string `yaml:"private_key_file"`

	// PSKMasterFile is the local path to a shared PSK master secret used
	// to derive per-link preshared keys via HKDF. Optional — if empty, no
	// PSK is applied. The same file must be present (with identical
	// content) on every fat node for derived keys to match.
	PSKMasterFile string `yaml:"psk_master_file"`

	// HealthFile is the path to write a JSON health status file.
	// Monitoring tools can watch this file for staleness.
	// Default: /var/run/meshctl/health.json
	HealthFile string `yaml:"health_file"`

	// HealthAddr is an optional HTTP address to serve health status.
	// Example: ":9474". If empty, no HTTP health endpoint is started.
	HealthAddr string `yaml:"health_addr"`
}

// RepoConfig describes how the agent fetches its config.
type RepoConfig struct {
	Sources      []SourceConfig `yaml:"sources"`
	FetchTimeout time.Duration  `yaml:"fetch_timeout"`
	LocalCache   string         `yaml:"local_cache"`
}

// SourceConfig describes a single config source.
type SourceConfig struct {
	Type     string `yaml:"type"`     // "git", "http", "local"
	URL      string `yaml:"url"`
	Branch   string `yaml:"branch"`
	SSHKey   string `yaml:"ssh_key"`
	Path     string `yaml:"path"`
	TokenEnv string `yaml:"token_env"`
}

// LoadAgentConfig reads and parses agent.yaml.
func LoadAgentConfig(path string) (*AgentConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading agent config: %w", err)
	}
	var cfg AgentConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing agent config: %w", err)
	}
	cfg.setDefaults()
	return &cfg, nil
}

func (c *AgentConfig) setDefaults() {
	if c.ConfigSyncInterval == 0 {
		c.ConfigSyncInterval = 5 * time.Minute
	}
	if c.ProbeInterval == 0 {
		c.ProbeInterval = 30 * time.Second
	}
	if c.ProbePort == 0 {
		c.ProbePort = 9473
	}
	if c.BIRDSocket == "" {
		c.BIRDSocket = "/var/run/bird/bird.ctl"
	}
	if c.BIRDIncludePath == "" {
		c.BIRDIncludePath = "/etc/bird/meshctl.conf"
	}
	if c.PrivateKeyFile == "" {
		c.PrivateKeyFile = "/etc/meshctl/wireguard.key"
	}
	if c.Repo.FetchTimeout == 0 {
		c.Repo.FetchTimeout = 30 * time.Second
	}
	if c.Repo.LocalCache == "" {
		c.Repo.LocalCache = "/etc/meshctl/cache/"
	}
	if c.HealthFile == "" {
		c.HealthFile = "/var/run/meshctl/health.json"
	}
}

// Fetcher retrieves node config from the config repo.
type Fetcher struct {
	cfg    *AgentConfig
	logger *slog.Logger
	debug  bool

	// Backoff state for repeated failures.
	consecutiveFailures int
	lastFetchTime       time.Time
}

// NewFetcher creates a config fetcher.
func NewFetcher(cfg *AgentConfig, logger *slog.Logger, debug bool) *Fetcher {
	return &Fetcher{cfg: cfg, logger: logger, debug: debug}
}

// maxBackoff is the maximum delay added by exponential backoff.
const maxBackoff = 10 * time.Minute

// backoffDelay returns the current backoff delay based on consecutive failures.
// Uses exponential backoff: 0, 30s, 60s, 120s, 240s, ... capped at maxBackoff.
func (f *Fetcher) backoffDelay() time.Duration {
	if f.consecutiveFailures <= 0 {
		return 0
	}
	delay := 30 * time.Second
	for i := 1; i < f.consecutiveFailures && delay < maxBackoff; i++ {
		delay *= 2
	}
	if delay > maxBackoff {
		delay = maxBackoff
	}
	return delay
}

// Fetch tries each source in order. On success, copies files to local cache.
// Returns the directory containing the node's config files.
// Applies exponential backoff when all sources fail repeatedly.
func (f *Fetcher) Fetch(ctx context.Context) (string, error) {
	// Apply backoff delay if we've had consecutive failures.
	if delay := f.backoffDelay(); delay > 0 {
		f.logger.Info("backing off before fetch", "delay", delay, "consecutive_failures", f.consecutiveFailures)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(delay):
		}
	}

	for _, src := range f.cfg.Repo.Sources {
		dir, err := f.fetchSource(ctx, src)
		if err != nil {
			f.logger.Warn("source failed", "type", src.Type, "error", err)
			continue
		}
		// Copy to cache.
		if err := f.updateCache(dir); err != nil {
			f.logger.Warn("cache update failed", "error", err)
		}
		f.consecutiveFailures = 0
		return dir, nil
	}

	// All sources failed — increment backoff counter.
	f.consecutiveFailures++

	// Try cache.
	cacheDir := filepath.Join(f.cfg.Repo.LocalCache, f.cfg.NodeName)
	if _, err := os.Stat(cacheDir); err == nil {
		f.logger.Warn("all sources failed, using cached config",
			"consecutive_failures", f.consecutiveFailures)
		return cacheDir, nil
	}

	return "", fmt.Errorf("all config sources failed and no cache available (attempt %d)", f.consecutiveFailures)
}

func (f *Fetcher) fetchSource(ctx context.Context, src SourceConfig) (string, error) {
	switch src.Type {
	case "git":
		return f.fetchGit(ctx, src)
	case "http":
		return f.fetchHTTP(ctx, src)
	case "local":
		return f.fetchLocal(src)
	default:
		return "", fmt.Errorf("unknown source type: %s", src.Type)
	}
}

// fetchGit clones/pulls a git repo and returns the node output directory.
func (f *Fetcher) fetchGit(ctx context.Context, src SourceConfig) (string, error) {
	repoDir := filepath.Join(os.TempDir(), "meshctl-repo")

	sshEnv := ""
	if src.SSHKey != "" {
		sshVerbose := ""
		if f.debug {
			sshVerbose = "-vvv "
		}
		sshEnv = fmt.Sprintf("GIT_SSH_COMMAND=ssh %s-i %s -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=/etc/meshctl/known_hosts", sshVerbose, src.SSHKey)
	}

	setSSH := func(cmd *exec.Cmd) {
		if sshEnv != "" {
			cmd.Env = append(os.Environ(), sshEnv)
		}
	}

	if _, err := os.Stat(filepath.Join(repoDir, ".git")); err == nil {
		// Repo exists — fetch + reset to handle force pushes.
		branch := src.Branch
		if branch == "" {
			branch = "main"
		}

		fetchCmd := exec.CommandContext(ctx, "git", "-C", repoDir, "fetch", "--depth", "1", "origin", branch)
		setSSH(fetchCmd)
		if out, err := fetchCmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("git fetch: %w: %s", err, string(out))
		}

		resetCmd := exec.CommandContext(ctx, "git", "-C", repoDir, "reset", "--hard", "FETCH_HEAD")
		if out, err := resetCmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("git reset: %w: %s", err, string(out))
		}
	} else {
		// Fresh clone.
		args := []string{"clone", "--depth", "1"}
		if src.Branch != "" {
			args = append(args, "-b", src.Branch)
		}
		args = append(args, src.URL, repoDir)
		cmd := exec.CommandContext(ctx, "git", args...)
		setSSH(cmd)
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("git clone: %w: %s", err, string(out))
		}
	}

	nodeDir := filepath.Join(repoDir, "output", f.cfg.NodeName)
	if _, err := os.Stat(nodeDir); err != nil {
		return "", fmt.Errorf("node directory not found: %s", nodeDir)
	}
	return nodeDir, nil
}

// fetchHTTP downloads individual files from an HTTP base URL.
func (f *Fetcher) fetchHTTP(ctx context.Context, src SourceConfig) (string, error) {
	destDir := filepath.Join(os.TempDir(), "meshctl-http", f.cfg.NodeName)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("creating http dest dir: %w", err)
	}

	// Known file list for a fat node.
	files := []string{"bird-meshctl.conf", "wireguard.json"}

	client := &http.Client{Timeout: f.cfg.Repo.FetchTimeout}
	for _, file := range files {
		url := fmt.Sprintf("%s%s/%s", src.URL, f.cfg.NodeName, file)
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return "", fmt.Errorf("creating request: %w", err)
		}
		if src.TokenEnv != "" {
			if token := os.Getenv(src.TokenEnv); token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
		}
		resp, err := client.Do(req)
		if err != nil {
			return "", fmt.Errorf("fetching %s: %w", url, err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return "", fmt.Errorf("fetching %s: status %d", url, resp.StatusCode)
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", url, err)
		}
		if err := os.WriteFile(filepath.Join(destDir, file), data, 0o644); err != nil {
			return "", fmt.Errorf("writing %s: %w", file, err)
		}
	}
	return destDir, nil
}

// fetchLocal returns the local directory path if it exists.
func (f *Fetcher) fetchLocal(src SourceConfig) (string, error) {
	nodeDir := filepath.Join(src.Path, f.cfg.NodeName)
	if _, err := os.Stat(nodeDir); err != nil {
		// Try without node name subdirectory.
		if _, err := os.Stat(src.Path); err != nil {
			return "", fmt.Errorf("local path not found: %s", src.Path)
		}
		return src.Path, nil
	}
	return nodeDir, nil
}

// updateCache copies the fetched config to the local cache directory.
func (f *Fetcher) updateCache(srcDir string) error {
	dstDir := filepath.Join(f.cfg.Repo.LocalCache, f.cfg.NodeName)
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return err
	}

	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return err
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(srcDir, e.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dstDir, e.Name()), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}
