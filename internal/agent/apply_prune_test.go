package agent

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func testApplier(t *testing.T) *Applier {
	t.Helper()
	return &Applier{
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		stateFile: filepath.Join(t.TempDir(), "managed-ifaces.json"),
	}
}

func TestManagedIfaces_RoundTrip(t *testing.T) {
	a := testApplier(t)
	a.writeManagedIfaces(map[string]bool{"igp-b": true, "igp-a": true})
	got := a.readManagedIfaces()
	want := []string{"igp-a", "igp-b"} // sorted
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("round trip: got %v, want %v", got, want)
	}
}

func TestReadManagedIfaces_MissingAndCorrupt(t *testing.T) {
	a := testApplier(t)
	// Missing file → nil, no panic.
	if got := a.readManagedIfaces(); got != nil {
		t.Errorf("missing state: got %v, want nil", got)
	}
	// Corrupt file → nil (treated as nothing managed), no panic.
	if err := os.WriteFile(a.stateFile, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := a.readManagedIfaces(); got != nil {
		t.Errorf("corrupt state: got %v, want nil", got)
	}
}

// TestPruneStaleInterfaces_ReconcilesState verifies the bookkeeping: after an
// interface leaves the desired set, it is removed from the state file. The
// actual `ip link del` is gated behind `wg show`, which fails for the
// non-existent test interfaces, so nothing is really deleted — but the state
// must still be reconciled to the new desired set.
func TestPruneStaleInterfaces_ReconcilesState(t *testing.T) {
	a := testApplier(t)
	a.writeManagedIfaces(map[string]bool{"igp-a": true, "igp-b": true})

	a.pruneStaleInterfaces(map[string]bool{"igp-a": true})

	got := a.readManagedIfaces()
	if len(got) != 1 || got[0] != "igp-a" {
		t.Fatalf("after prune: got %v, want [igp-a]", got)
	}
}

// TestPruneStaleInterfaces_FirstRunNoPrune verifies that with no prior state
// file (fresh node or pre-upgrade), nothing is pruned and the desired set is
// recorded for next time. This is the safety property: the agent never
// deletes an interface it has not previously recorded as its own.
func TestPruneStaleInterfaces_FirstRunNoPrune(t *testing.T) {
	a := testApplier(t)
	if _, err := os.Stat(a.stateFile); !os.IsNotExist(err) {
		t.Fatal("expected no state file at start")
	}
	a.pruneStaleInterfaces(map[string]bool{"igp-a": true, "igp-c": true})
	got := a.readManagedIfaces()
	if len(got) != 2 || got[0] != "igp-a" || got[1] != "igp-c" {
		t.Fatalf("first run: got %v, want [igp-a igp-c]", got)
	}
}

func TestPruneStaleInterfaces_NoStateFileNoop(t *testing.T) {
	a := testApplier(t)
	a.stateFile = ""
	a.pruneStaleInterfaces(map[string]bool{"igp-a": true}) // must not panic
}

// TestApplyWireguard_SkipsUnmanaged verifies a peer flagged wg_unmanaged is not
// configured (no shelling out to ip/wg) and is never recorded as an
// agent-managed interface, so the prune reconciler will never delete it.
func TestApplyWireguard_SkipsUnmanaged(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "wireguard.key")
	if err := os.WriteFile(keyFile, []byte("dummy-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	wg := `{"node":"HKG","peers":[{"name":"bohouse","public_key":"k=",` +
		`"interface":"igpc-bohouse","unmanaged":true,"peer_type":"routeros"}]}`
	if err := os.WriteFile(filepath.Join(dir, "wireguard.json"), []byte(wg), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &Applier{
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		privateKeyFile: keyFile,
		stateFile:      filepath.Join(dir, "managed-ifaces.json"),
	}
	if err := a.applyWireguard(dir); err != nil {
		t.Fatalf("applyWireguard: %v", err)
	}
	if got := a.readManagedIfaces(); len(got) != 0 {
		t.Errorf("unmanaged interface must not be tracked as agent-managed; got %v", got)
	}
}
