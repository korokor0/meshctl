package bird

import (
	"testing"
)

func TestIsDigits(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"0001", true},
		{"9999", true},
		{"", false},
		{"000a", false},
		{"12 4", false},
	}
	for _, tt := range tests {
		if got := isDigits(tt.input); got != tt.want {
			t.Errorf("isDigits(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}
