// Package psk derives per-link WireGuard preshared keys from a shared
// master secret using HKDF-SHA256.
//
// Both endpoints of a link independently derive the same PSK by feeding
// the sorted node-pair names as HKDF info. No coordination is required —
// only a shared master secret file distributed to each fat node out of band.
package psk

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"sort"
	"strings"
)

// KeyLength is the WireGuard preshared key length in bytes.
const KeyLength = 32

// Derive computes a deterministic 32-byte PSK from the master secret and
// the sorted node-pair. The two node names are sorted lexicographically
// so Derive(m, "A", "B") == Derive(m, "B", "A").
func Derive(master []byte, nodeA, nodeB string) [KeyLength]byte {
	pair := sortedPair(nodeA, nodeB)
	info := []byte("meshctl-psk-v1|" + pair)

	// HKDF-SHA256 Extract: PRK = HMAC-SHA256(salt=zero, IKM=master).
	// Salt is deliberately empty (zero-filled) — info carries the context.
	mac := hmac.New(sha256.New, make([]byte, sha256.Size))
	mac.Write(master)
	prk := mac.Sum(nil)

	// HKDF-SHA256 Expand: one block is enough for a 32-byte key.
	// T(1) = HMAC-SHA256(PRK, info || 0x01).
	mac = hmac.New(sha256.New, prk)
	mac.Write(info)
	mac.Write([]byte{0x01})
	t1 := mac.Sum(nil)

	var out [KeyLength]byte
	copy(out[:], t1[:KeyLength])
	return out
}

// DeriveBase64 returns the derived key as a base64 string (WireGuard format).
func DeriveBase64(master []byte, nodeA, nodeB string) string {
	key := Derive(master, nodeA, nodeB)
	return base64.StdEncoding.EncodeToString(key[:])
}

// LoadMaster reads a master secret from disk. The file may contain raw
// bytes or base64-encoded content; base64 is detected by a trailing newline
// and valid decoding.
func LoadMaster(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading psk master: %w", err)
	}
	trimmed := strings.TrimSpace(string(data))
	if decoded, err := base64.StdEncoding.DecodeString(trimmed); err == nil && len(decoded) >= 16 {
		return decoded, nil
	}
	if len(data) < 16 {
		return nil, fmt.Errorf("psk master too short (%d bytes, need ≥16)", len(data))
	}
	return data, nil
}

func sortedPair(a, b string) string {
	pair := []string{a, b}
	sort.Strings(pair)
	return pair[0] + "|" + pair[1]
}
