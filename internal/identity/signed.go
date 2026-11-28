package identity

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Signature domains. A signature made for one kind of statement can never be
// replayed as another.
const (
	domainCert   = "ourchive/v1/cert"
	domainRoster = "ourchive/v1/roster"
)

// Signed is a signed statement. The signature covers the exact payload bytes
// (prefixed by the domain and a zero byte), so verification never depends on
// re-encoding JSON the same way.
type Signed struct {
	Payload []byte `json:"payload"`
	Sig     []byte `json:"sig"`
}

var (
	ErrBadSignature = errors.New("identity: signature doesn't verify")
	ErrWrongHub     = errors.New("identity: statement is for a different hub")
)

func signedMessage(domain string, payload []byte) []byte {
	msg := make([]byte, 0, len(domain)+1+len(payload))
	msg = append(msg, domain...)
	msg = append(msg, 0)
	return append(msg, payload...)
}

func sign(domain string, key ed25519.PrivateKey, v any) (Signed, error) {
	payload, err := json.Marshal(v)
	if err != nil {
		return Signed{}, err
	}
	return Signed{Payload: payload, Sig: ed25519.Sign(key, signedMessage(domain, payload))}, nil
}

func verify(domain string, pub ed25519.PublicKey, s Signed) error {
	if len(pub) != ed25519.PublicKeySize || len(s.Sig) != ed25519.SignatureSize {
		return ErrBadSignature
	}
	if !ed25519.Verify(pub, signedMessage(domain, s.Payload), s.Sig) {
		return ErrBadSignature
	}
	return nil
}

// decodeStrict parses JSON and rejects unknown fields and trailing data.
func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("identity: trailing data after statement")
	}
	return nil
}

// DeviceKind says what a device key may do.
type DeviceKind string

const (
	// KindDevice is an app install that holds (or can unlock) the identity.
	KindDevice DeviceKind = "device"
	// KindServer is a headless host with a host-only key and no identity access.
	KindServer DeviceKind = "server"
)

func (k DeviceKind) valid() bool { return k == KindDevice || k == KindServer }

// MaxNameLength caps device names, in bytes.
const MaxNameLength = 128

// Cert says that a device key belongs to an account on one hub. It is signed
// by the account key for that hub.
type Cert struct {
	HubID         string            `json:"hub_id"`
	AccountPubkey ed25519.PublicKey `json:"account_pubkey"`
	DevicePubkey  ed25519.PublicKey `json:"device_pubkey"`
	Kind          DeviceKind        `json:"kind"`
	Name          string            `json:"name"`
	IssuedAt      time.Time         `json:"issued_at"`
}

func (c *Cert) validate() error {
	if err := ValidateHubID(c.HubID); err != nil {
		return err
	}
	if len(c.AccountPubkey) != ed25519.PublicKeySize || len(c.DevicePubkey) != ed25519.PublicKeySize {
		return errors.New("identity: certificate has a malformed key")
	}
	if !c.Kind.valid() {
		return fmt.Errorf("identity: unknown device kind %q", c.Kind)
	}
	if len(c.Name) > MaxNameLength {
		return errors.New("identity: device name is too long")
	}
	if c.IssuedAt.IsZero() {
		return errors.New("identity: certificate has no issue time")
	}
	return nil
}

// IssueCert signs a certificate for devicePub with the account key. The
// account public key and hub ID in the certificate come from the arguments.
func IssueCert(account ed25519.PrivateKey, hubID string, devicePub ed25519.PublicKey, kind DeviceKind, name string, now time.Time) (Signed, error) {
	c := Cert{
		HubID:         hubID,
		AccountPubkey: account.Public().(ed25519.PublicKey),
		DevicePubkey:  devicePub,
		Kind:          kind,
		Name:          name,
		IssuedAt:      now.UTC().Truncate(time.Second),
	}
	if err := c.validate(); err != nil {
		return Signed{}, err
	}
	return sign(domainCert, account, &c)
}

// VerifyCert checks the signature against the account key named inside the
// certificate and that the certificate is for hubID. The caller must still
// check that the account key is an active key of the right user, and that
// the device is in the latest roster and not revoked.
func VerifyCert(s Signed, hubID string) (*Cert, error) {
	var c Cert
	if err := decodeStrict(s.Payload, &c); err != nil {
		return nil, fmt.Errorf("identity: malformed certificate: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	if err := verify(domainCert, c.AccountPubkey, s); err != nil {
		return nil, err
	}
	if c.HubID != hubID {
		return nil, ErrWrongHub
	}
	return &c, nil
}

// RosterDevice is one entry in a device list.
type RosterDevice struct {
	Pubkey ed25519.PublicKey `json:"pubkey"`
	Kind   DeviceKind        `json:"kind"`
	Name   string            `json:"name"`
}

// Roster is a person's device list for one hub. Versions only go up; hubs
// ignore anything not newer than what they already have.
type Roster struct {
	HubID         string            `json:"hub_id"`
	AccountPubkey ed25519.PublicKey `json:"account_pubkey"`
	Version       uint64            `json:"version"`
	Devices       []RosterDevice    `json:"devices"`
	IssuedAt      time.Time         `json:"issued_at"`
}

// MaxRosterDevices caps the size of a device list.
const MaxRosterDevices = 256

func (r *Roster) validate() error {
	if err := ValidateHubID(r.HubID); err != nil {
		return err
	}
	if len(r.AccountPubkey) != ed25519.PublicKeySize {
		return errors.New("identity: roster has a malformed account key")
	}
	if r.Version == 0 {
		return errors.New("identity: roster version must start at 1")
	}
	if len(r.Devices) > MaxRosterDevices {
		return errors.New("identity: roster has too many devices")
	}
	if r.IssuedAt.IsZero() {
		return errors.New("identity: roster has no issue time")
	}
	seen := make(map[string]bool, len(r.Devices))
	for _, d := range r.Devices {
		if len(d.Pubkey) != ed25519.PublicKeySize {
			return errors.New("identity: roster has a malformed device key")
		}
		if !d.Kind.valid() {
			return fmt.Errorf("identity: unknown device kind %q", d.Kind)
		}
		if len(d.Name) > MaxNameLength {
			return errors.New("identity: device name is too long")
		}
		if seen[string(d.Pubkey)] {
			return errors.New("identity: roster lists a device twice")
		}
		seen[string(d.Pubkey)] = true
	}
	return nil
}

// Contains reports whether devicePub is listed.
func (r *Roster) Contains(devicePub ed25519.PublicKey) bool {
	return r.Device(devicePub) != nil
}

// Device returns the entry for devicePub, or nil.
func (r *Roster) Device(devicePub ed25519.PublicKey) *RosterDevice {
	for i := range r.Devices {
		if r.Devices[i].Pubkey.Equal(devicePub) {
			return &r.Devices[i]
		}
	}
	return nil
}

// SignRoster signs r with the account key. r.HubID, r.Version, r.Devices and
// r.IssuedAt must be set; the account public key is filled in from account.
func SignRoster(account ed25519.PrivateKey, r Roster) (Signed, error) {
	r.AccountPubkey = account.Public().(ed25519.PublicKey)
	r.IssuedAt = r.IssuedAt.UTC().Truncate(time.Second)
	if r.Devices == nil {
		r.Devices = []RosterDevice{}
	}
	if err := r.validate(); err != nil {
		return Signed{}, err
	}
	return sign(domainRoster, account, &r)
}

// VerifyRoster checks the signature against the account key named inside the
// roster and that it is for hubID. The caller must check that the account key
// is the user's active key and compare versions with NewerThan.
func VerifyRoster(s Signed, hubID string) (*Roster, error) {
	var r Roster
	if err := decodeStrict(s.Payload, &r); err != nil {
		return nil, fmt.Errorf("identity: malformed roster: %w", err)
	}
	if err := r.validate(); err != nil {
		return nil, err
	}
	if err := verify(domainRoster, r.AccountPubkey, s); err != nil {
		return nil, err
	}
	if r.HubID != hubID {
		return nil, ErrWrongHub
	}
	return &r, nil
}

// NewerThan reports whether r should replace prev. A nil prev is always
// older. Rosters from a different account key are never newer: moving to a
// new key goes through the move protocol, not a roster update.
func (r *Roster) NewerThan(prev *Roster) bool {
	if prev == nil {
		return true
	}
	return r.HubID == prev.HubID && r.AccountPubkey.Equal(prev.AccountPubkey) && r.Version > prev.Version
}
