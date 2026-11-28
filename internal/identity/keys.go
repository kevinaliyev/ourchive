package identity

import (
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Derivation labels. Every derivation carries a version prefix so the scheme
// can change later without colliding with v1 keys.
const (
	labelMaster  = "ourchive/v1/master"
	labelHub     = "ourchive/v1/hub/"
	labelDevice  = "ourchive/v1/device/"
	labelFileKey = "ourchive/v1/file-key"
)

// SecretSize is the size of a master secret or a device secret.
const SecretSize = 32

// MasterSecret is the 32-byte secret every per-hub account key derives from.
type MasterSecret [SecretSize]byte

// MarshalJSON encodes the secret as base64, like other byte fields.
func (m MasterSecret) MarshalJSON() ([]byte, error) { return json.Marshal(m[:]) }

// UnmarshalJSON decodes a base64 secret of exactly SecretSize bytes.
func (m *MasterSecret) UnmarshalJSON(data []byte) error {
	var b []byte
	if err := json.Unmarshal(data, &b); err != nil {
		return err
	}
	defer clear(b)
	if len(b) != SecretSize {
		return fmt.Errorf("identity: master secret must be %d bytes", SecretSize)
	}
	copy(m[:], b)
	return nil
}

// DeviceSecret is the 32 random bytes held by one app install. It is never
// derived from the master secret.
type DeviceSecret [SecretSize]byte

// NewDeviceSecret returns a fresh random device secret.
func NewDeviceSecret() (DeviceSecret, error) {
	var d DeviceSecret
	_, err := rand.Read(d[:])
	return d, err
}

func masterFromSeed(seed []byte) MasterSecret {
	var m MasterSecret
	copy(m[:], hkdfKey(seed, labelMaster, SecretSize))
	return m
}

// hkdfKey is HKDF-SHA256 with an empty salt.
func hkdfKey(secret []byte, info string, n int) []byte {
	k, err := hkdf.Key(sha256.New, secret, nil, info, n)
	if err != nil {
		panic(err) // n is always small and fixed
	}
	return k
}

// AccountKey derives the Ed25519 account key for one hub and persona
// (0 is the default persona):
// HKDF-SHA256(master, info "ourchive/v1/hub/<hub_id>/<persona>") → seed.
func (m *MasterSecret) AccountKey(hubID string, persona uint32) (ed25519.PrivateKey, error) {
	if err := ValidateHubID(hubID); err != nil {
		return nil, err
	}
	info := labelHub + hubID + "/" + strconv.FormatUint(uint64(persona), 10)
	seed := hkdfKey(m[:], info, ed25519.SeedSize)
	defer clear(seed)
	return ed25519.NewKeyFromSeed(seed), nil
}

// DeviceKey derives this device's Ed25519 key for one hub:
// HKDF-SHA256(device_secret, info "ourchive/v1/device/<hub_id>") → seed.
// A different key per hub stops one device from linking two hubs.
func (d *DeviceSecret) DeviceKey(hubID string) (ed25519.PrivateKey, error) {
	if err := ValidateHubID(hubID); err != nil {
		return nil, err
	}
	seed := hkdfKey(d[:], labelDevice+hubID, ed25519.SeedSize)
	defer clear(seed)
	return ed25519.NewKeyFromSeed(seed), nil
}

// HubIDLength is the number of characters in a hub ID.
const HubIDLength = 26

var hubIDEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// HubID computes a hub's ID from its Ed25519 public key: the first 26
// characters of lowercase, unpadded base32(SHA-256(pubkey)), which is 130 bits.
func HubID(hubPub ed25519.PublicKey) (string, error) {
	if len(hubPub) != ed25519.PublicKeySize {
		return "", fmt.Errorf("identity: hub public key must be %d bytes", ed25519.PublicKeySize)
	}
	sum := sha256.Sum256(hubPub)
	return hubIDEncoding.EncodeToString(sum[:])[:HubIDLength], nil
}

var ErrHubID = errors.New("identity: malformed hub ID")

// ValidateHubID checks that s looks like a hub ID (26 lowercase base32 characters).
func ValidateHubID(s string) error {
	if len(s) != HubIDLength {
		return ErrHubID
	}
	for _, c := range s {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyz234567", c) {
			return ErrHubID
		}
	}
	return nil
}

// CheckHubID reports whether hubID belongs to hubPub. The app uses it when it
// pins a hub on first join and on every later connection.
func CheckHubID(hubID string, hubPub ed25519.PublicKey) bool {
	want, err := HubID(hubPub)
	return err == nil && want == hubID
}
