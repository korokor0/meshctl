// Package cost implements the quantized cost band state machine with
// hysteresis for OSPF link cost decisions based on one-way delay measurements.
package cost

import (
	"time"
)

// Band defines a single cost level with hysteresis thresholds.
type Band struct {
	UpThreshold   time.Duration // forward delay must exceed this to enter from below
	DownThreshold time.Duration // forward delay must drop below this to leave going down
	Cost          uint32
	HoldCount     int // consecutive probes in new band before switching
}

// LinkState tracks the cost band state for a single peer link.
type LinkState struct {
	ForwardDelay     time.Duration // EWMA-smoothed one-way forward delay
	RTT              time.Duration // EWMA-smoothed RTT (for fallback)
	CurrentBand      int
	PendingBand      int
	HoldCounter      int
	Failures         int
	StaticCost       *uint32 // if set, used instead of band-based cost
	FallbackCost     *uint32 // if set, used instead of penalty on failure
	BandwidthPenalty uint32  // additive cost from low bandwidth (0 if above threshold)
}

// Engine evaluates probe results against cost bands with hysteresis.
type Engine struct {
	bands        []Band
	penaltyCost  uint32
	alpha        float64 // EWMA smoothing factor
	failureLimit int     // consecutive failures before penalty
	state        map[string]*LinkState
}

// NewEngine creates a cost engine with the given bands and parameters.
func NewEngine(bands []Band, penaltyCost uint32, alpha float64, failureLimit int) *Engine {
	return &Engine{
		bands:        bands,
		penaltyCost:  penaltyCost,
		alpha:        alpha,
		failureLimit: failureLimit,
		state:        make(map[string]*LinkState),
	}
}

// getState returns the state for a peer, creating it if needed.
func (e *Engine) getState(peer string) *LinkState {
	s, ok := e.state[peer]
	if !ok {
		s = &LinkState{}
		e.state[peer] = s
	}
	return s
}

// State returns the current link state for a peer, or nil if unknown.
func (e *Engine) State(peer string) *LinkState {
	return e.state[peer]
}

// SetStaticCost configures a peer to always use a fixed OSPF cost,
// bypassing the band state machine entirely. Probing is not needed.
func (e *Engine) SetStaticCost(peer string, cost uint32) {
	s := e.getState(peer)
	s.StaticCost = &cost
}

// SetFallbackCost configures a per-peer fallback cost that is used instead
// of the global penalty cost when the peer becomes unreachable. This allows
// thin/static peers to degrade gracefully to a known cost rather than
// jumping to 65535.
func (e *Engine) SetFallbackCost(peer string, cost uint32) {
	s := e.getState(peer)
	s.FallbackCost = &cost
}

// SetBandwidthPenalty sets the additive bandwidth penalty for a peer link.
// This is computed from the Cisco-style auto-cost formula at config load time.
func (e *Engine) SetBandwidthPenalty(peer string, penalty uint32) {
	s := e.getState(peer)
	s.BandwidthPenalty = penalty
}

// CalcBandwidthPenalty computes the bandwidth penalty for a link.
// Uses Cisco-style auto-cost formula:
//
//	penalty = reference_bw/link_bw - reference_bw/threshold
//
// The returned value is applied multiplicatively by the cost engine:
//
//	cost = band_cost * (1000 + penalty*10) / 1000
//
// So penalty=20 means +20% cost, penalty=50 means +50%.
// Returns 0 if linkBwMbps >= thresholdMbps.
func CalcBandwidthPenalty(linkBwMbps, thresholdMbps, referenceMbps int) uint32 {
	if linkBwMbps >= thresholdMbps {
		return 0
	}
	if linkBwMbps <= 0 {
		linkBwMbps = 1 // avoid division by zero
	}
	penalty := referenceMbps/linkBwMbps - referenceMbps/thresholdMbps
	if penalty < 0 {
		return 0
	}
	return uint32(penalty)
}

// UpdateConfig replaces the engine's bands, penalty cost, EWMA alpha, and
// failure limit. Existing per-peer state (current band, EWMA values) is
// preserved — only the parameters change. This allows hot-reloading cost
// bands from updated config without losing probe history.
func (e *Engine) UpdateConfig(bands []Band, penaltyCost uint32, alpha float64, failureLimit int) {
	e.bands = bands
	e.penaltyCost = penaltyCost
	e.alpha = alpha
	e.failureLimit = failureLimit
	// Clamp any peer whose CurrentBand exceeds the new band count.
	for _, s := range e.state {
		if s.CurrentBand >= len(bands) {
			s.CurrentBand = len(bands) - 1
		}
		if s.PendingBand >= len(bands) {
			s.PendingBand = s.CurrentBand
			s.HoldCounter = 0
		}
	}
}

// Snapshot returns a copy of all peer states and their current costs.
func (e *Engine) Snapshot() map[string]PeerSnapshot {
	result := make(map[string]PeerSnapshot, len(e.state))
	for peer, s := range e.state {
		result[peer] = PeerSnapshot{
			ForwardDelay:     s.ForwardDelay,
			RTT:              s.RTT,
			CurrentBand:      s.CurrentBand,
			PendingBand:      s.PendingBand,
			HoldCounter:      s.HoldCounter,
			Failures:         s.Failures,
			BandwidthPenalty: s.BandwidthPenalty,
			Cost:             e.CurrentCost(peer),
			IsStatic:         s.StaticCost != nil,
		}
	}
	return result
}

// PeerSnapshot is a read-only snapshot of a single peer's cost state.
type PeerSnapshot struct {
	ForwardDelay     time.Duration
	RTT              time.Duration
	CurrentBand      int
	PendingBand      int
	HoldCounter      int
	Failures         int
	BandwidthPenalty uint32
	Cost             uint32
	IsStatic         bool
}

// IsStatic returns true if the peer has a static cost configured.
func (e *Engine) IsStatic(peer string) bool {
	s, ok := e.state[peer]
	return ok && s.StaticCost != nil
}

// CurrentCost returns the OSPF cost for a peer.
// Priority: static cost > band-based cost > fallback cost > penalty cost.
func (e *Engine) CurrentCost(peer string) uint32 {
	s, ok := e.state[peer]
	if !ok {
		return e.penaltyCost
	}
	// Static cost always wins — no probing involved.
	if s.StaticCost != nil {
		return *s.StaticCost
	}
	// Unreachable: use per-peer fallback if set, otherwise global penalty.
	if s.Failures >= e.failureLimit {
		if s.FallbackCost != nil {
			return *s.FallbackCost
		}
		return e.penaltyCost
	}
	if s.CurrentBand < 0 || s.CurrentBand >= len(e.bands) {
		return e.penaltyCost
	}
	cost := e.bands[s.CurrentBand].Cost
	if s.BandwidthPenalty > 0 {
		cost = cost * (1000 + s.BandwidthPenalty*10) / 1000
	}
	return cost
}

// RecordSuccess processes a successful probe measurement for a peer.
// forwardDelay is the measured one-way delay; rtt is the round-trip time.
// If forwardDelay <= 0, rtt/2 is used as fallback.
// Returns true if the cost band changed.
func (e *Engine) RecordSuccess(peer string, forwardDelay, rtt time.Duration) bool {
	s := e.getState(peer)
	s.Failures = 0

	// Determine the delay value for cost band evaluation.
	delay := forwardDelay
	if delay <= 0 {
		delay = rtt / 2
	}

	// EWMA update.
	if s.ForwardDelay == 0 {
		s.ForwardDelay = delay
	} else {
		s.ForwardDelay = ewma(s.ForwardDelay, delay, e.alpha)
	}
	if s.RTT == 0 {
		s.RTT = rtt
	} else {
		s.RTT = ewma(s.RTT, rtt, e.alpha)
	}

	// Evaluate band transition.
	return e.evaluateBand(s)
}

// RecordFailure records a probe failure for a peer.
// Returns true if the cost changed (i.e., peer became unreachable).
func (e *Engine) RecordFailure(peer string) bool {
	s := e.getState(peer)
	s.Failures++
	if s.Failures == e.failureLimit {
		return true // transitioned to penalty
	}
	return false
}

// evaluateBand checks whether the smoothed delay warrants a band change,
// applying hysteresis and hold count.
func (e *Engine) evaluateBand(s *LinkState) bool {
	delay := s.ForwardDelay
	target := s.CurrentBand

	// Check if delay has risen above a higher band's UpThreshold.
	for i := s.CurrentBand + 1; i < len(e.bands); i++ {
		if delay >= e.bands[i].UpThreshold {
			target = i
		} else {
			break
		}
	}

	// Check if delay has dropped below a lower band's DownThreshold.
	if target == s.CurrentBand {
		for i := s.CurrentBand - 1; i >= 0; i-- {
			if i+1 < len(e.bands) && delay < e.bands[i+1].DownThreshold {
				target = i
			} else {
				break
			}
		}
	}

	if target == s.CurrentBand {
		// No change — reset pending state.
		s.PendingBand = s.CurrentBand
		s.HoldCounter = 0
		return false
	}

	// Apply hold count: must see consecutive probes in the new band.
	if target == s.PendingBand {
		s.HoldCounter++
	} else {
		s.PendingBand = target
		s.HoldCounter = 1
	}

	required := e.bands[target].HoldCount
	if s.HoldCounter >= required {
		oldBand := s.CurrentBand
		s.CurrentBand = target
		s.PendingBand = target
		s.HoldCounter = 0
		return oldBand != s.CurrentBand
	}
	return false
}

// ewma computes an exponentially weighted moving average update.
func ewma(current, sample time.Duration, alpha float64) time.Duration {
	return time.Duration(alpha*float64(sample) + (1-alpha)*float64(current))
}
