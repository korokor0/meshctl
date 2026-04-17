package cost

import (
	"testing"
	"time"
)

func defaultBands() []Band {
	return []Band{
		{UpThreshold: 0, DownThreshold: 0, Cost: 20, HoldCount: 5},
		{UpThreshold: 4 * time.Millisecond, DownThreshold: 2 * time.Millisecond, Cost: 80, HoldCount: 5},
		{UpThreshold: 12 * time.Millisecond, DownThreshold: 8 * time.Millisecond, Cost: 160, HoldCount: 5},
		{UpThreshold: 30 * time.Millisecond, DownThreshold: 20 * time.Millisecond, Cost: 250, HoldCount: 5},
		{UpThreshold: 60 * time.Millisecond, DownThreshold: 40 * time.Millisecond, Cost: 350, HoldCount: 5},
		{UpThreshold: 100 * time.Millisecond, DownThreshold: 70 * time.Millisecond, Cost: 480, HoldCount: 5},
		{UpThreshold: 160 * time.Millisecond, DownThreshold: 120 * time.Millisecond, Cost: 640, HoldCount: 5},
		{UpThreshold: 220 * time.Millisecond, DownThreshold: 180 * time.Millisecond, Cost: 840, HoldCount: 5},
		{UpThreshold: 300 * time.Millisecond, DownThreshold: 260 * time.Millisecond, Cost: 1100, HoldCount: 5},
	}
}

func TestInitialCost(t *testing.T) {
	e := NewEngine(defaultBands(), 65535, 0.3, 3)
	// Unknown peer returns penalty cost.
	if got := e.CurrentCost("unknown"); got != 65535 {
		t.Errorf("expected 65535, got %d", got)
	}

	// After first probe at low delay, should be in band 0.
	e.RecordSuccess("peer1", 1*time.Millisecond, 2*time.Millisecond)
	if got := e.CurrentCost("peer1"); got != 20 {
		t.Errorf("expected cost 20, got %d", got)
	}
}

func TestBandTransitionUp(t *testing.T) {
	e := NewEngine(defaultBands(), 65535, 1.0, 3) // alpha=1.0 for instant tracking
	peer := "peer1"

	// Start in band 0.
	e.RecordSuccess(peer, 1*time.Millisecond, 2*time.Millisecond)
	if e.CurrentCost(peer) != 20 {
		t.Fatalf("expected band 0 cost 20, got %d", e.CurrentCost(peer))
	}

	// Send probes at 5ms (above band 1 UpThreshold of 4ms).
	// Need HoldCount=5 consecutive probes.
	for i := 0; i < 4; i++ {
		e.RecordSuccess(peer, 5*time.Millisecond, 10*time.Millisecond)
		if e.CurrentCost(peer) != 20 {
			t.Fatalf("probe %d: should still be band 0 (hold count not met)", i)
		}
	}
	// 5th probe should trigger transition.
	changed := e.RecordSuccess(peer, 5*time.Millisecond, 10*time.Millisecond)
	if !changed {
		t.Fatal("expected band change on 5th probe")
	}
	if e.CurrentCost(peer) != 80 {
		t.Errorf("expected cost 80, got %d", e.CurrentCost(peer))
	}
}

func TestBandTransitionDown(t *testing.T) {
	e := NewEngine(defaultBands(), 65535, 1.0, 3)
	peer := "peer1"

	// Move to band 1 first.
	e.RecordSuccess(peer, 1*time.Millisecond, 2*time.Millisecond)
	for i := 0; i < 5; i++ {
		e.RecordSuccess(peer, 5*time.Millisecond, 10*time.Millisecond)
	}
	if e.CurrentCost(peer) != 80 {
		t.Fatalf("setup: expected cost 80, got %d", e.CurrentCost(peer))
	}

	// Now send probes below band 1 DownThreshold (2ms).
	for i := 0; i < 4; i++ {
		e.RecordSuccess(peer, 1*time.Millisecond, 2*time.Millisecond)
		if e.CurrentCost(peer) != 80 {
			t.Fatalf("probe %d: should still be band 1 (hold count not met)", i)
		}
	}
	changed := e.RecordSuccess(peer, 1*time.Millisecond, 2*time.Millisecond)
	if !changed {
		t.Fatal("expected band change on 5th low probe")
	}
	if e.CurrentCost(peer) != 20 {
		t.Errorf("expected cost 20, got %d", e.CurrentCost(peer))
	}
}

func TestHysteresis(t *testing.T) {
	e := NewEngine(defaultBands(), 65535, 1.0, 3)
	peer := "peer1"

	// Start in band 0.
	e.RecordSuccess(peer, 1*time.Millisecond, 2*time.Millisecond)

	// Probe at 3ms — above band 1 DownThreshold (2ms) but below UpThreshold (4ms).
	// Should NOT transition up.
	for i := 0; i < 10; i++ {
		e.RecordSuccess(peer, 3*time.Millisecond, 6*time.Millisecond)
	}
	if e.CurrentCost(peer) != 20 {
		t.Errorf("hysteresis failed: should still be band 0 at 3ms, got cost %d", e.CurrentCost(peer))
	}
}

func TestFailurePenalty(t *testing.T) {
	e := NewEngine(defaultBands(), 65535, 0.3, 3)
	peer := "peer1"

	e.RecordSuccess(peer, 1*time.Millisecond, 2*time.Millisecond)
	if e.CurrentCost(peer) != 20 {
		t.Fatalf("expected cost 20, got %d", e.CurrentCost(peer))
	}

	// First two failures: no penalty yet.
	e.RecordFailure(peer)
	e.RecordFailure(peer)
	if e.CurrentCost(peer) == 65535 {
		t.Error("penalty applied too early")
	}

	// Third failure triggers penalty.
	changed := e.RecordFailure(peer)
	if !changed {
		t.Error("expected change on 3rd failure")
	}
	if e.CurrentCost(peer) != 65535 {
		t.Errorf("expected penalty 65535, got %d", e.CurrentCost(peer))
	}

	// Recovery: a successful probe resets failures.
	e.RecordSuccess(peer, 1*time.Millisecond, 2*time.Millisecond)
	if e.CurrentCost(peer) == 65535 {
		t.Error("cost should recover after success")
	}
}

func TestFallbackRTTHalf(t *testing.T) {
	e := NewEngine(defaultBands(), 65535, 1.0, 3)
	peer := "peer1"

	// Forward delay <= 0 should use rtt/2.
	e.RecordSuccess(peer, 0, 10*time.Millisecond)
	s := e.State(peer)
	if s.ForwardDelay != 5*time.Millisecond {
		t.Errorf("expected 5ms (rtt/2), got %v", s.ForwardDelay)
	}
}

func TestStaticCost(t *testing.T) {
	e := NewEngine(defaultBands(), 65535, 0.3, 3)
	peer := "thin-peer"

	// Set static cost — should always return that cost.
	e.SetStaticCost(peer, 150)
	if got := e.CurrentCost(peer); got != 150 {
		t.Errorf("expected static cost 150, got %d", got)
	}

	// Static cost is immune to probe results.
	e.RecordSuccess(peer, 50*time.Millisecond, 100*time.Millisecond)
	if got := e.CurrentCost(peer); got != 150 {
		t.Errorf("static cost should not change after probe, got %d", got)
	}

	// Static cost is immune to failures.
	for i := 0; i < 5; i++ {
		e.RecordFailure(peer)
	}
	if got := e.CurrentCost(peer); got != 150 {
		t.Errorf("static cost should not change after failures, got %d", got)
	}

	// IsStatic should return true.
	if !e.IsStatic(peer) {
		t.Error("expected IsStatic=true")
	}
	if e.IsStatic("other") {
		t.Error("expected IsStatic=false for unknown peer")
	}
}

func TestFallbackCost(t *testing.T) {
	e := NewEngine(defaultBands(), 65535, 0.3, 3)
	peer := "thin-peer"

	// Set fallback cost and start with a successful probe.
	e.SetFallbackCost(peer, 200)
	e.RecordSuccess(peer, 1*time.Millisecond, 2*time.Millisecond)
	if got := e.CurrentCost(peer); got != 20 {
		t.Errorf("expected band 0 cost 20, got %d", got)
	}

	// After 3 failures, should use fallback (200), not penalty (65535).
	e.RecordFailure(peer)
	e.RecordFailure(peer)
	e.RecordFailure(peer)
	if got := e.CurrentCost(peer); got != 200 {
		t.Errorf("expected fallback cost 200, got %d", got)
	}

	// Recovery works normally.
	e.RecordSuccess(peer, 1*time.Millisecond, 2*time.Millisecond)
	if got := e.CurrentCost(peer); got == 200 || got == 65535 {
		t.Errorf("expected band cost after recovery, got %d", got)
	}
}

func TestFallbackCost_NotSet(t *testing.T) {
	e := NewEngine(defaultBands(), 65535, 0.3, 3)
	peer := "peer-no-fallback"

	e.RecordSuccess(peer, 1*time.Millisecond, 2*time.Millisecond)
	e.RecordFailure(peer)
	e.RecordFailure(peer)
	e.RecordFailure(peer)

	// Without fallback, should use global penalty.
	if got := e.CurrentCost(peer); got != 65535 {
		t.Errorf("expected penalty 65535, got %d", got)
	}
}

func TestCalcBandwidthPenalty(t *testing.T) {
	tests := []struct {
		linkBw    int
		threshold int
		reference int
		want      uint32
	}{
		{1000, 300, 1000, 0},   // above threshold
		{300, 300, 1000, 0},    // at threshold
		{200, 300, 1000, 2},    // 1000/200 - 1000/300 = 5 - 3 = 2
		{100, 300, 1000, 7},    // 1000/100 - 1000/300 = 10 - 3 = 7
		{50, 300, 1000, 17},    // 1000/50 - 1000/300 = 20 - 3 = 17
		{0, 300, 1000, 997},    // linkBw=0 → clamped to 1: 1000/1 - 1000/300 = 1000 - 3 = 997
	}
	for _, tt := range tests {
		got := CalcBandwidthPenalty(tt.linkBw, tt.threshold, tt.reference)
		if got != tt.want {
			t.Errorf("CalcBandwidthPenalty(%d, %d, %d) = %d, want %d",
				tt.linkBw, tt.threshold, tt.reference, got, tt.want)
		}
	}
}

func TestBandwidthPenaltyMultiplicative(t *testing.T) {
	e := NewEngine(defaultBands(), 65535, 1.0, 3)
	peer := "low-bw-peer"

	// Set bandwidth penalty=50 → +50%: cost * (1000+500) / 1000.
	e.SetBandwidthPenalty(peer, 50)

	// Band 0 cost 20 * 1.5 = 30.
	e.RecordSuccess(peer, 1*time.Millisecond, 2*time.Millisecond)
	if got := e.CurrentCost(peer); got != 30 {
		t.Errorf("expected 20*1.5=30, got %d", got)
	}

	// Move to band 1 (cost 80) * 1.5 = 120.
	for i := 0; i < 5; i++ {
		e.RecordSuccess(peer, 5*time.Millisecond, 10*time.Millisecond)
	}
	if got := e.CurrentCost(peer); got != 120 {
		t.Errorf("expected 80*1.5=120, got %d", got)
	}
}

func TestEWMA(t *testing.T) {
	result := ewma(100*time.Millisecond, 200*time.Millisecond, 0.3)
	// 0.3*200 + 0.7*100 = 60 + 70 = 130ms
	expected := 130 * time.Millisecond
	if result != expected {
		t.Errorf("expected %v, got %v", expected, result)
	}
}
