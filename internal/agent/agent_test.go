package agent

import (
	"testing"
	"time"
)

func TestLoadAgentConfig_Defaults(t *testing.T) {
	cfg := &AgentConfig{
		NodeName: "test-node",
	}
	cfg.setDefaults()

	if cfg.ConfigSyncInterval != 5*time.Minute {
		t.Errorf("expected 5m config sync, got %v", cfg.ConfigSyncInterval)
	}
	if cfg.ProbeInterval != 30*time.Second {
		t.Errorf("expected 30s probe interval, got %v", cfg.ProbeInterval)
	}
	if cfg.ProbePort != 9473 {
		t.Errorf("expected probe port 9473, got %d", cfg.ProbePort)
	}
	if cfg.BIRDSocket != "/var/run/bird/bird.ctl" {
		t.Errorf("expected default bird socket, got %s", cfg.BIRDSocket)
	}
	if cfg.BIRDIncludePath != "/etc/bird/meshctl.conf" {
		t.Errorf("expected default bird include, got %s", cfg.BIRDIncludePath)
	}
	if cfg.Repo.FetchTimeout != 30*time.Second {
		t.Errorf("expected 30s fetch timeout, got %v", cfg.Repo.FetchTimeout)
	}
}

func TestParseChronyTracking(t *testing.T) {
	output := `Reference ID    : A9FEA97B (time.cloudflare.com)
Stratum         : 3
Ref time (UTC)  : Wed Apr 16 10:30:00 2026
System time     : 0.000001234 seconds fast of NTP time
Last offset     : +0.000234567 seconds
RMS offset      : 0.000500000 seconds
Frequency       : 12.345 ppm slow
Residual freq   : +0.001 ppm
Skew            : 0.123 ppm
Root delay      : 0.023456789 seconds
Root dispersion : 0.001234567 seconds
Update interval : 64.0 seconds
Leap status     : Normal
`
	offset, synced, err := parseChronyTracking(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !synced {
		t.Error("expected synced=true for 'Leap status : Normal'")
	}
	// offset should be ~234.567 microseconds
	if offset < 200*time.Microsecond || offset > 300*time.Microsecond {
		t.Errorf("offset=%v, expected ~234µs", offset)
	}
}

func TestParseChronyTracking_NotSynced(t *testing.T) {
	output := `Leap status     : Not synchronised
Last offset     : +0.500000000 seconds
`
	_, synced, _ := parseChronyTracking(output)
	if synced {
		t.Error("expected synced=false for 'Not synchronised'")
	}
}

func TestReplaceBIRDCosts(t *testing.T) {
	config := `# BIRD config
protocol ospf v3 meshctl_ospf3_v4 {
    ipv4 { table igptable4; import all; export all; };
    instance id 64;
    area 0.0.0.0 {
        interface "igp-jp-relay" {
            type pointopoint;
            hello 10;
            dead 40;
            cost 100;
        };
        interface "igp-us-west" {
            type pointopoint;
            hello 10;
            dead 40;
            cost 200;
        };
    };
}
`
	costMap := map[string]uint32{
		"igp-jp-relay": 50,
		"igp-us-west":  500,
	}

	result := replaceBIRDCosts(config, costMap)

	// Verify costs were replaced.
	if !containsStr(result, "cost 50;") {
		t.Error("expected 'cost 50;' for igp-jp-relay")
	}
	if !containsStr(result, "cost 500;") {
		t.Error("expected 'cost 500;' for igp-us-west")
	}
	if containsStr(result, "cost 100;") {
		t.Error("old cost 100 should have been replaced")
	}
	if containsStr(result, "cost 200;") {
		t.Error("old cost 200 should have been replaced")
	}

	// Verify structure is preserved.
	if !containsStr(result, "protocol ospf") {
		t.Error("protocol header should be preserved")
	}
	if !containsStr(result, "hello 10;") {
		t.Error("hello should be preserved")
	}
}

func TestReplaceBIRDCosts_UnknownInterface(t *testing.T) {
	config := `    interface "igp-unknown" {
        cost 100;
    };`

	// No matching interface in costMap — cost should remain unchanged.
	costMap := map[string]uint32{"igp-other": 50}
	result := replaceBIRDCosts(config, costMap)
	if !containsStr(result, "cost 100;") {
		t.Error("unknown interface cost should remain unchanged")
	}
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestExtractHost(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"10.200.255.1/32", "10.200.255.1"},
		{"192.168.1.0/24", "192.168.1.0"},
		{"10.0.0.1", "10.0.0.1"},
		{"169.254.0.2/31", "169.254.0.2"},
	}
	for _, tt := range tests {
		got := extractHost(tt.input)
		if got != tt.want {
			t.Errorf("extractHost(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
