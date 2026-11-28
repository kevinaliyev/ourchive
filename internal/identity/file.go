package identity

import (
	"bytes"
	"crypto/rand"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

// Identity is the decrypted content of an identity file.
type Identity struct {
	MasterSecret MasterSecret      `json:"master_secret"`
	Profile      Profile           `json:"profile"`
	Hubs         []HubMembership   `json:"hubs"`
	Rosters      map[string]Signed `json:"rosters"` // hub_id → latest signed roster
	CreatedAt    time.Time         `json:"created_at"`
}

// Profile is the default profile sent to hubs when joining with it.
type Profile struct {
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
	Avatar      []byte `json:"avatar,omitempty"`
}

// HubMembership records one hub this identity has joined.
type HubMembership struct {
	HubID           string    `json:"hub_id"`
	HubPubkey       []byte    `json:"hub_pubkey"`
	URL             string    `json:"url"`
	Name            string    `json:"name"`
	JoinedAt        time.Time `json:"joined_at"`
	Persona         uint32    `json:"persona"`
	ProfileOverride *Profile  `json:"profile_override,omitempty"`
}

// Wipe zeroes the master secret. Call it when the identity is no longer needed.
func (id *Identity) Wipe() { clear(id.MasterSecret[:]) }

// FileMode says what unlocks an identity file.
type FileMode string

const (
	// ModePassword: the identity password alone. The app warns about it.
	ModePassword FileMode = "password"
	// ModePasswordSecret: the password plus a 128-bit secret key.
	ModePasswordSecret FileMode = "password+secret"
)

// KDFParams are the Argon2id parameters. They are stored in the file header
// so they can be raised later without breaking old files.
type KDFParams struct {
	Memory  uint32 `json:"m"` // KiB
	Time    uint32 `json:"t"`
	Threads uint8  `json:"p"`
}

// DefaultKDFParams: m=128 MiB, t=3, p=1.
var DefaultKDFParams = KDFParams{Memory: 128 * 1024, Time: 3, Threads: 1}

// Upper bounds accepted when opening a file, so a crafted header can't make
// the app allocate unbounded memory or spin for hours.
const (
	maxKDFMemory  = 1024 * 1024 // 1 GiB in KiB, 8× the default
	maxKDFTime    = 64
	maxKDFThreads = 64
)

func (p KDFParams) validate() error {
	if p.Memory < 8*uint32(p.Threads) || p.Memory > maxKDFMemory || p.Time < 1 || p.Time > maxKDFTime || p.Threads < 1 || p.Threads > maxKDFThreads {
		return errors.New("identity: identity file has unsupported KDF parameters")
	}
	return nil
}

// File format, version 1:
//
//	magic "OURCHIVE-ID" | version byte (1) | header length (uint32 BE) | header JSON |
//	nonce (24 bytes) | XChaCha20-Poly1305 ciphertext of the Identity JSON
//
// Everything before the nonce is authenticated as associated data.
const (
	fileMagic     = "OURCHIVE-ID"
	fileVersion   = 1
	saltSize      = 16
	maxHeaderSize = 4096
)

type fileHeader struct {
	KDF  string `json:"kdf"`
	Salt []byte `json:"salt"`
	KDFParams
	Mode FileMode `json:"mode"`
}

var (
	ErrNotIdentityFile = errors.New("identity: not an Ourchive identity file")
	ErrFileVersion     = errors.New("identity: identity file was made by a newer version of Ourchive")
	ErrUnlock          = errors.New("identity: wrong password or secret key, or the file is damaged")
	ErrNeedSecretKey   = errors.New("identity: this identity file also needs its secret key")
	ErrWeakPassword    = fmt.Errorf("identity: a password-only identity file needs a password of at least %d characters", MinPasswordOnlyLength)
	ErrEmptyPassword   = errors.New("identity: the identity password can't be empty")
)

// MinPasswordOnlyLength is the strength floor for password-only files.
const MinPasswordOnlyLength = 12

// SecretKey is the 128-bit secret key used in password-plus-secret mode.
type SecretKey [16]byte

var secretKeyEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewSecretKey returns a fresh random secret key.
func NewSecretKey() (SecretKey, error) {
	var k SecretKey
	_, err := rand.Read(k[:])
	return k, err
}

// String formats the key for printing: 26 base32 characters in groups of five,
// for example "ABCDE-FGHIJ-KLMNO-PQRST-UVWXY-Z".
func (k SecretKey) String() string {
	s := secretKeyEncoding.EncodeToString(k[:])
	var b strings.Builder
	for i := 0; i < len(s); i += 5 {
		if i > 0 {
			b.WriteByte('-')
		}
		b.WriteString(s[i:min(i+5, len(s))])
	}
	return b.String()
}

// ParseSecretKey reads a key typed by the user. Dashes, spaces and case are ignored.
func ParseSecretKey(s string) (SecretKey, error) {
	s = strings.ToUpper(strings.NewReplacer("-", "", " ", "", "\t", "").Replace(s))
	var k SecretKey
	raw, err := secretKeyEncoding.DecodeString(s)
	if err != nil || len(raw) != len(k) || secretKeyEncoding.EncodeToString(raw) != s {
		return k, errors.New("identity: malformed secret key")
	}
	copy(k[:], raw)
	return k, nil
}

// SealOptions control how an identity file is written.
type SealOptions struct {
	Password string
	// SecretKey, when set, selects password-plus-secret mode.
	SecretKey *SecretKey
	// KDF defaults to DefaultKDFParams.
	KDF *KDFParams
}

// Seal encrypts id into identity file bytes.
func Seal(id *Identity, opts SealOptions) ([]byte, error) {
	return seal(rand.Reader, id, opts)
}

func seal(rnd io.Reader, id *Identity, opts SealOptions) ([]byte, error) {
	if opts.Password == "" {
		return nil, ErrEmptyPassword
	}
	mode := ModePassword
	if opts.SecretKey != nil {
		mode = ModePasswordSecret
	} else if utf8.RuneCountInString(opts.Password) < MinPasswordOnlyLength {
		return nil, ErrWeakPassword
	}
	params := DefaultKDFParams
	if opts.KDF != nil {
		params = *opts.KDF
	}
	if err := params.validate(); err != nil {
		return nil, err
	}
	h := fileHeader{KDF: "argon2id", Salt: make([]byte, saltSize), KDFParams: params, Mode: mode}
	if _, err := io.ReadFull(rnd, h.Salt); err != nil {
		return nil, err
	}
	nonce := make([]byte, chacha20poly1305.NonceSizeX)
	if _, err := io.ReadFull(rnd, nonce); err != nil {
		return nil, err
	}

	hdr, err := json.Marshal(&h)
	if err != nil {
		return nil, err
	}
	ad := make([]byte, 0, len(fileMagic)+5+len(hdr))
	ad = append(ad, fileMagic...)
	ad = append(ad, fileVersion)
	ad = binary.BigEndian.AppendUint32(ad, uint32(len(hdr)))
	ad = append(ad, hdr...)

	body := *id
	if body.Hubs == nil {
		body.Hubs = []HubMembership{}
	}
	if body.Rosters == nil {
		body.Rosters = map[string]Signed{}
	}
	plain, err := json.Marshal(&body)
	if err != nil {
		return nil, err
	}
	defer clear(plain)

	key := fileKey(&h, opts.Password, opts.SecretKey)
	defer clear(key)
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	out := append(ad, nonce...)
	return aead.Seal(out, nonce, plain, ad), nil
}

func fileKey(h *fileHeader, password string, sk *SecretKey) []byte {
	k := argon2.IDKey([]byte(password), h.Salt, h.Time, h.Memory, h.Threads, 32)
	if h.Mode != ModePasswordSecret {
		return k
	}
	ikm := make([]byte, 0, len(k)+len(sk))
	ikm = append(append(ikm, k...), sk[:]...)
	clear(k)
	defer clear(ikm)
	return hkdfKey(ikm, labelFileKey, 32)
}

// FileInfo is the plain header of an identity file, readable without the password.
type FileInfo struct {
	Mode FileMode
	KDF  KDFParams
}

// Inspect reads the plain header, so the app knows whether to ask for the secret key.
func Inspect(data []byte) (FileInfo, error) {
	h, _, _, err := parseFile(data)
	if err != nil {
		return FileInfo{}, err
	}
	return FileInfo{Mode: h.Mode, KDF: h.KDFParams}, nil
}

func parseFile(data []byte) (h fileHeader, ad, rest []byte, err error) {
	if !bytes.HasPrefix(data, []byte(fileMagic)) {
		return h, nil, nil, ErrNotIdentityFile
	}
	p := data[len(fileMagic):]
	if len(p) < 5 {
		return h, nil, nil, ErrNotIdentityFile
	}
	if p[0] != fileVersion {
		if p[0] > fileVersion {
			return h, nil, nil, ErrFileVersion
		}
		return h, nil, nil, ErrNotIdentityFile
	}
	n := binary.BigEndian.Uint32(p[1:5])
	if n > maxHeaderSize || uint64(len(p)) < 5+uint64(n) {
		return h, nil, nil, ErrNotIdentityFile
	}
	hdr := p[5 : 5+n]
	if err := decodeStrict(hdr, &h); err != nil {
		return h, nil, nil, ErrNotIdentityFile
	}
	if h.KDF != "argon2id" || len(h.Salt) != saltSize || (h.Mode != ModePassword && h.Mode != ModePasswordSecret) {
		return h, nil, nil, ErrNotIdentityFile
	}
	if err := h.KDFParams.validate(); err != nil {
		return h, nil, nil, err
	}
	adLen := len(fileMagic) + 5 + int(n)
	return h, data[:adLen], data[adLen:], nil
}

// Open decrypts identity file bytes. sk is required for password-plus-secret
// files and ignored otherwise.
func Open(data []byte, password string, sk *SecretKey) (*Identity, error) {
	h, ad, rest, err := parseFile(data)
	if err != nil {
		return nil, err
	}
	if h.Mode == ModePasswordSecret && sk == nil {
		return nil, ErrNeedSecretKey
	}
	if len(rest) < chacha20poly1305.NonceSizeX+chacha20poly1305.Overhead {
		return nil, ErrUnlock
	}
	nonce, ct := rest[:chacha20poly1305.NonceSizeX], rest[chacha20poly1305.NonceSizeX:]
	key := fileKey(&h, password, sk)
	defer clear(key)
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	plain, err := aead.Open(nil, nonce, ct, ad)
	if err != nil {
		return nil, ErrUnlock
	}
	defer clear(plain)
	var id Identity
	if err := json.Unmarshal(plain, &id); err != nil {
		return nil, fmt.Errorf("identity: identity file body is malformed: %w", err)
	}
	return &id, nil
}
