package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// HealthStatus represents the agent's current health state.
type HealthStatus struct {
	NodeName        string    `json:"node_name"`
	StartedAt       time.Time `json:"started_at"`
	LastFetchOK     time.Time `json:"last_fetch_ok,omitempty"`
	LastFetchError  string    `json:"last_fetch_error,omitempty"`
	ConfigAge       string    `json:"config_age"`
	LastProbeRound  time.Time `json:"last_probe_round,omitempty"`
	PeerCount       int       `json:"peer_count"`
	NTPSynced       *bool     `json:"ntp_synced,omitempty"`
	NTPOffset       string    `json:"ntp_offset,omitempty"`
}

// HealthTracker tracks agent health metrics and writes them to a file
// and/or serves them via HTTP.
type HealthTracker struct {
	mu          sync.RWMutex
	nodeName    string
	startedAt   time.Time
	lastFetchOK time.Time
	lastFetchErr string
	lastProbe   time.Time
	peerCount   int
	ntpSynced   *bool
	ntpOffset   time.Duration
	filePath    string
	logger      *slog.Logger

	// peersFunc is called by the /peers HTTP endpoint to get live peer data.
	// Set by Agent after construction.
	peersFunc func() []PeerStatus
}

// SetPeersFunc registers a callback that returns per-peer status snapshots.
func (h *HealthTracker) SetPeersFunc(fn func() []PeerStatus) {
	h.peersFunc = fn
}

// NewHealthTracker creates a health tracker.
func NewHealthTracker(nodeName, filePath string, logger *slog.Logger) *HealthTracker {
	return &HealthTracker{
		nodeName:  nodeName,
		startedAt: time.Now(),
		filePath:  filePath,
		logger:    logger,
	}
}

// RecordFetchSuccess records a successful config fetch.
func (h *HealthTracker) RecordFetchSuccess() {
	h.mu.Lock()
	h.lastFetchOK = time.Now()
	h.lastFetchErr = ""
	h.mu.Unlock()
	h.writeFile()
}

// RecordFetchError records a failed config fetch.
func (h *HealthTracker) RecordFetchError(err error) {
	h.mu.Lock()
	h.lastFetchErr = err.Error()
	h.mu.Unlock()
	h.writeFile()
}

// RecordProbeRound records a completed probe round.
func (h *HealthTracker) RecordProbeRound(peerCount int) {
	h.mu.Lock()
	h.lastProbe = time.Now()
	h.peerCount = peerCount
	h.mu.Unlock()
	h.writeFile()
}

// RecordNTPStatus records NTP synchronization status.
func (h *HealthTracker) RecordNTPStatus(synced bool, offset time.Duration) {
	h.mu.Lock()
	h.ntpSynced = &synced
	h.ntpOffset = offset
	h.mu.Unlock()
}

// Status returns the current health status snapshot.
func (h *HealthTracker) Status() HealthStatus {
	h.mu.RLock()
	defer h.mu.RUnlock()

	status := HealthStatus{
		NodeName:   h.nodeName,
		StartedAt:  h.startedAt,
		PeerCount:  h.peerCount,
		NTPSynced:  h.ntpSynced,
	}

	if !h.lastFetchOK.IsZero() {
		status.LastFetchOK = h.lastFetchOK
		status.ConfigAge = time.Since(h.lastFetchOK).Truncate(time.Second).String()
	} else {
		status.ConfigAge = "never"
	}
	if h.lastFetchErr != "" {
		status.LastFetchError = h.lastFetchErr
	}
	if !h.lastProbe.IsZero() {
		status.LastProbeRound = h.lastProbe
	}
	if h.ntpOffset != 0 {
		status.NTPOffset = h.ntpOffset.String()
	}

	return status
}

// writeFile writes the health status to the configured file path.
func (h *HealthTracker) writeFile() {
	if h.filePath == "" {
		return
	}

	status := h.Status()
	data, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		h.logger.Warn("failed to marshal health status", "error", err)
		return
	}

	// Ensure parent directory exists.
	dir := filepath.Dir(h.filePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.logger.Warn("failed to create health file dir", "error", err, "dir", dir)
		return
	}

	if err := atomicWriteFile(h.filePath, append(data, '\n'), 0o644); err != nil {
		h.logger.Warn("failed to write health file", "error", err, "path", h.filePath)
	}
}

// WritePeers writes per-peer status to a JSON file alongside the health file.
// The file path is derived from the health file path: health.json → peers.json.
func (h *HealthTracker) WritePeers(peers []PeerStatus) {
	if h.filePath == "" {
		return
	}
	peersPath := peersFilePath(h.filePath)

	data, err := json.MarshalIndent(peers, "", "  ")
	if err != nil {
		h.logger.Warn("failed to marshal peers status", "error", err)
		return
	}
	if err := atomicWriteFile(peersPath, append(data, '\n'), 0o644); err != nil {
		h.logger.Warn("failed to write peers file", "error", err, "path", peersPath)
	}
}

// PeersFilePath returns the path to the peers status file.
func (h *HealthTracker) PeersFilePath() string {
	return peersFilePath(h.filePath)
}

func peersFilePath(healthPath string) string {
	dir := filepath.Dir(healthPath)
	return filepath.Join(dir, "peers.json")
}

// WriteShutdown writes a final health status indicating the agent has stopped.
func (h *HealthTracker) WriteShutdown() {
	h.mu.Lock()
	h.lastFetchErr = "agent shutting down"
	h.mu.Unlock()
	h.writeFile()
}

// ServeHTTP starts an HTTP health endpoint. Blocks until ctx is cancelled.
func (h *HealthTracker) ServeHTTP(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		status := h.Status()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(status)
	})
	mux.HandleFunc("/peers", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if h.peersFunc == nil {
			json.NewEncoder(w).Encode([]struct{}{})
			return
		}
		json.NewEncoder(w).Encode(h.peersFunc())
	})

	srv := &http.Server{Addr: addr, Handler: mux}

	go func() {
		<-ctx.Done()
		srv.Close()
	}()

	h.logger.Info("starting health HTTP endpoint", "addr", addr)
	err := srv.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return fmt.Errorf("health HTTP server: %w", err)
}
