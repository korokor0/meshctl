package psk

import (
	"bytes"
	"testing"
)

func TestDeriveSymmetric(t *testing.T) {
	master := []byte("this-is-a-test-master-secret-32b")
	ab := Derive(master, "hk-core", "jp-relay")
	ba := Derive(master, "jp-relay", "hk-core")
	if !bytes.Equal(ab[:], ba[:]) {
		t.Fatalf("derivation must be symmetric; got %x vs %x", ab, ba)
	}
}

func TestDeriveDistinct(t *testing.T) {
	master := []byte("this-is-a-test-master-secret-32b")
	k1 := Derive(master, "hk-core", "jp-relay")
	k2 := Derive(master, "hk-core", "sg-relay")
	if bytes.Equal(k1[:], k2[:]) {
		t.Fatalf("different pairs must yield different keys")
	}
}

func TestDeriveDeterministic(t *testing.T) {
	master := []byte("this-is-a-test-master-secret-32b")
	k1 := Derive(master, "a", "b")
	k2 := Derive(master, "a", "b")
	if !bytes.Equal(k1[:], k2[:]) {
		t.Fatalf("derivation must be deterministic")
	}
}

func TestDeriveMasterDependent(t *testing.T) {
	k1 := Derive([]byte("master-secret-one-xxxxxxxxxxxxxx"), "a", "b")
	k2 := Derive([]byte("master-secret-two-xxxxxxxxxxxxxx"), "a", "b")
	if bytes.Equal(k1[:], k2[:]) {
		t.Fatalf("different masters must yield different keys")
	}
}

func TestDeriveBase64Length(t *testing.T) {
	got := DeriveBase64([]byte("master-secret-abcdefghijklmnopqr"), "a", "b")
	// 32 bytes → 44 base64 chars with padding.
	if len(got) != 44 {
		t.Fatalf("expected 44 base64 chars, got %d (%q)", len(got), got)
	}
}
