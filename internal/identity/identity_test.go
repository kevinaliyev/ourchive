package identity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestWordlistHash(t *testing.T) {
	sum := sha256.Sum256([]byte(strings.ReplaceAll(wordlistText, "\r", "")))
	if got := hex.EncodeToString(sum[:]); got != "2f5eed53a4727b4bf8880d8f3f199efc90e58503646d9ff8eff3a2ed3b24dbda" {
		t.Fatalf("wordlist hash = %s", got)
	}
}

// Official BIP39 vectors (Trezor), passphrase "TREZOR".
func TestBIP39Vectors(t *testing.T) {
	cases := []struct{ entropy, mnemonic, seed string }{
		{
			"00000000000000000000000000000000",
			"abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about",
			"c55257c360c07c72029aebc1b53c05ed0362ada38ead3e3e9efa3708e53495531f09a6987599d18264c1e1c92f2cf141630c7a3c4ab7c81b2f001698e7463b04",
		},
		{
			"0000000000000000000000000000000000000000000000000000000000000000",
			"abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon art",
			"bda85446c68413707090a52022edd26a1c9462295029f2e60cd7c4f2bbd3097170af7a4d73245cafa9c3cca8d561a7c3de6f5d4a10be8ed2a5e608d68f92fcc8",
		},
	}
	for _, c := range cases {
		words := entropyToWords(unhex(t, c.entropy))
		if got := strings.Join(words, " "); got != c.mnemonic {
			t.Errorf("mnemonic = %q, want %q", got, c.mnemonic)
		}
		ent, err := wordsToEntropy(strings.Fields(c.mnemonic))
		if err != nil || hex.EncodeToString(ent) != c.entropy {
			t.Errorf("entropy = %x, %v", ent, err)
		}
		if got := hex.EncodeToString(bip39Seed(c.mnemonic, "TREZOR")); got != c.seed {
			t.Errorf("seed = %s, want %s", got, c.seed)
		}
	}
}

// Ourchive v1 derivation vectors. The seeds were computed independently
// (Python hashlib/hmac) from the definitions in the design doc §5.
const (
	vecPhrase     = "abandon amount liar amount expire adjust cage candy arch gather drum bullet absurd math era live bid rhythm alien crouch range attend journey unaware"
	vecPassphrase = "ourchive"
	vecSeed       = "7d0dd49b4a5d5e95f2c14b9227973788407f26c5d3bb60299b9481aeb613d69b608c04392fb74dbd993bc7e77b06c748b7ad107ee9f35e9dd7f456c7e8858b77"
	vecMaster     = "41f5851963e7e11a189fb9b8e6427402667c786b064f741ccc3ccabe010a0f15"
	vecHubID      = "4dtxuuduckysb5xn4ypwekk3dj" // hub pubkey = 32 × 0xaa
	vecAccount0   = "793582a3fd61fbb423c2740dd184dba1e3fbc5876a47d6d58721c5341b3f9508"
	vecAccount1   = "3440d82bd3b5615e11b6e03d913e78fa7bb4e0880a451647b6d28ffd019b11d9"
	vecDevice     = "2ce286d58b01cc219582be07899ab5d3617a3298a1029b73d98280aa93ad6f50" // device secret = 32 × 0x42
)

func vecEntropy() []byte {
	ent := make([]byte, 32)
	for i := range ent {
		ent[i] = byte(i)
	}
	return ent
}

func TestDerivationVectors(t *testing.T) {
	p, err := PhraseFromEntropy(vecEntropy())
	if err != nil {
		t.Fatal(err)
	}
	if p.String() != vecPhrase {
		t.Fatalf("phrase = %q", p)
	}
	if got := hex.EncodeToString(p.Seed(vecPassphrase)); got != vecSeed {
		t.Fatalf("seed = %s", got)
	}
	m := p.MasterSecret(vecPassphrase)
	if got := hex.EncodeToString(m[:]); got != vecMaster {
		t.Fatalf("master = %s", got)
	}

	hubID, err := HubID(ed25519.PublicKey(bytes.Repeat([]byte{0xaa}, 32)))
	if err != nil || hubID != vecHubID {
		t.Fatalf("hub id = %q, %v", hubID, err)
	}

	for persona, want := range []string{vecAccount0, vecAccount1} {
		k, err := m.AccountKey(hubID, uint32(persona))
		if err != nil {
			t.Fatal(err)
		}
		if got := hex.EncodeToString(k.Seed()); got != want {
			t.Errorf("account key persona %d seed = %s", persona, got)
		}
	}

	var d DeviceSecret
	copy(d[:], bytes.Repeat([]byte{0x42}, 32))
	dk, err := d.DeviceKey(hubID)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(dk.Seed()); got != vecDevice {
		t.Errorf("device key seed = %s", got)
	}
}

func TestKeysDifferPerHub(t *testing.T) {
	var m MasterSecret
	var d DeviceSecret
	a1, _ := m.AccountKey("aaaaaaaaaaaaaaaaaaaaaaaaaa", 0)
	a2, _ := m.AccountKey("bbbbbbbbbbbbbbbbbbbbbbbbbb", 0)
	d1, _ := d.DeviceKey("aaaaaaaaaaaaaaaaaaaaaaaaaa")
	d2, _ := d.DeviceKey("bbbbbbbbbbbbbbbbbbbbbbbbbb")
	if a1.Equal(a2) || d1.Equal(d2) {
		t.Fatal("keys for different hubs must differ")
	}
	if _, err := m.AccountKey("not-a-hub", 0); !errors.Is(err, ErrHubID) {
		t.Fatalf("bad hub id: %v", err)
	}
}

func TestCheckHubID(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	id, _ := HubID(pub)
	if !CheckHubID(id, pub) {
		t.Fatal("own id should match")
	}
	other, _, _ := ed25519.GenerateKey(nil)
	if CheckHubID(id, other) {
		t.Fatal("other key should not match")
	}
}

func TestParsePhrase(t *testing.T) {
	p, err := ParsePhrase("  " + strings.ToUpper(strings.ReplaceAll(vecPhrase, " ", "\n\t ")) + " ")
	if err != nil || p.String() != vecPhrase {
		t.Fatalf("parse = %q, %v", p, err)
	}
	if !bytes.Equal(p.Entropy(), vecEntropy()) {
		t.Fatal("entropy round trip")
	}

	words := strings.Fields(vecPhrase)
	if _, err := ParsePhrase(strings.Join(words[:23], " ")); !errors.Is(err, ErrPhraseLength) {
		t.Errorf("23 words: %v", err)
	}
	bad := append([]string(nil), words...)
	bad[3] = "notaword"
	if _, err := ParsePhrase(strings.Join(bad, " ")); !errors.Is(err, ErrPhraseWord) {
		t.Errorf("unknown word: %v", err)
	}
	swapped := append([]string(nil), words...)
	swapped[0], swapped[1] = swapped[1], swapped[0]
	if _, err := ParsePhrase(strings.Join(swapped, " ")); !errors.Is(err, ErrPhraseChecksum) {
		t.Errorf("swapped words: %v", err)
	}

	np, err := NewPhrase()
	if err != nil {
		t.Fatal(err)
	}
	if again, err := ParsePhrase(np.String()); err != nil || again.String() != np.String() {
		t.Fatalf("new phrase doesn't parse: %v", err)
	}
}

func TestPassphraseNormalization(t *testing.T) {
	p, _ := PhraseFromEntropy(vecEntropy())
	composed := p.Seed("café")
	decomposed := p.Seed("café")
	if !bytes.Equal(composed, decomposed) {
		t.Fatal("passphrase must be NFKD-normalized")
	}
	if bytes.Equal(composed, p.Seed("")) {
		t.Fatal("passphrase must change the seed")
	}
}

const testHub = "aaaaaaaaaaaaaaaaaaaaaaaaaa"

func testAccount(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	var m MasterSecret
	m[0] = 1
	k, err := m.AccountKey(testHub, 0)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestCert(t *testing.T) {
	acct := testAccount(t)
	devPub, _, _ := ed25519.GenerateKey(nil)
	now := time.Date(2026, 11, 28, 12, 0, 0, 500, time.UTC)

	s, err := IssueCert(acct, testHub, devPub, KindDevice, "Laptop", now)
	if err != nil {
		t.Fatal(err)
	}
	c, err := VerifyCert(s, testHub)
	if err != nil {
		t.Fatal(err)
	}
	if !c.DevicePubkey.Equal(devPub) || !c.AccountPubkey.Equal(acct.Public()) || c.Kind != KindDevice || c.Name != "Laptop" || !c.IssuedAt.Equal(now.Truncate(time.Second)) {
		t.Fatalf("cert = %+v", c)
	}

	if _, err := VerifyCert(s, "bbbbbbbbbbbbbbbbbbbbbbbbbb"); !errors.Is(err, ErrWrongHub) {
		t.Errorf("wrong hub: %v", err)
	}

	tampered := Signed{Payload: bytes.Replace(s.Payload, []byte("Laptop"), []byte("Lapdog"), 1), Sig: s.Sig}
	if _, err := VerifyCert(tampered, testHub); !errors.Is(err, ErrBadSignature) {
		t.Errorf("tampered: %v", err)
	}

	// A roster signature must not verify as a certificate.
	r, _ := SignRoster(acct, Roster{HubID: testHub, Version: 1, IssuedAt: now})
	if _, err := VerifyCert(Signed{Payload: s.Payload, Sig: r.Sig}, testHub); err == nil {
		t.Error("cross-domain signature accepted")
	}

	// Signed by someone other than the account key named inside.
	other := testAccountOther(t)
	forged := Signed{Payload: s.Payload, Sig: ed25519.Sign(other, signedMessage(domainCert, s.Payload))}
	if _, err := VerifyCert(forged, testHub); !errors.Is(err, ErrBadSignature) {
		t.Errorf("forged: %v", err)
	}

	if _, err := IssueCert(acct, testHub, devPub, "phone", "x", now); err == nil {
		t.Error("unknown kind accepted")
	}
	if _, err := IssueCert(acct, testHub, devPub, KindServer, strings.Repeat("n", MaxNameLength+1), now); err == nil {
		t.Error("long name accepted")
	}
}

func testAccountOther(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	var m MasterSecret
	m[0] = 2
	k, _ := m.AccountKey(testHub, 0)
	return k
}

func TestCertRejectsUnknownFields(t *testing.T) {
	acct := testAccount(t)
	payload := []byte(`{"hub_id":"` + testHub + `","account_pubkey":null,"extra":1}`)
	s := Signed{Payload: payload, Sig: ed25519.Sign(acct, signedMessage(domainCert, payload))}
	if _, err := VerifyCert(s, testHub); err == nil {
		t.Fatal("unknown field accepted")
	}
}

// A fixed certificate payload pins the wire format.
func TestCertVector(t *testing.T) {
	acct := testAccount(t)
	var d DeviceSecret
	dk, _ := d.DeviceKey(testHub)
	s, err := IssueCert(acct, testHub, dk.Public().(ed25519.PublicKey), KindServer, "NAS", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"hub_id":"aaaaaaaaaaaaaaaaaaaaaaaaaa","account_pubkey":"` + b64(acct.Public().(ed25519.PublicKey)) +
		`","device_pubkey":"` + b64(dk.Public().(ed25519.PublicKey)) + `","kind":"server","name":"NAS","issued_at":"2026-01-02T03:04:05Z"}`
	if string(s.Payload) != want {
		t.Fatalf("payload =\n%s\nwant\n%s", s.Payload, want)
	}
	// Ed25519 is deterministic, so the signature is fixed too.
	if !ed25519.Verify(acct.Public().(ed25519.PublicKey), signedMessage(domainCert, []byte(want)), s.Sig) {
		t.Fatal("signature")
	}
}

func TestRoster(t *testing.T) {
	acct := testAccount(t)
	d1, _, _ := ed25519.GenerateKey(nil)
	d2, _, _ := ed25519.GenerateKey(nil)
	now := time.Now()

	s1, err := SignRoster(acct, Roster{HubID: testHub, Version: 1, IssuedAt: now, Devices: []RosterDevice{{Pubkey: d1, Kind: KindDevice, Name: "Laptop"}}})
	if err != nil {
		t.Fatal(err)
	}
	r1, err := VerifyRoster(s1, testHub)
	if err != nil {
		t.Fatal(err)
	}
	if !r1.Contains(d1) || r1.Contains(d2) {
		t.Fatal("contains")
	}

	s2, _ := SignRoster(acct, Roster{HubID: testHub, Version: 2, IssuedAt: now, Devices: []RosterDevice{
		{Pubkey: d1, Kind: KindDevice, Name: "Laptop"},
		{Pubkey: d2, Kind: KindServer, Name: "NAS"},
	}})
	r2, err := VerifyRoster(s2, testHub)
	if err != nil {
		t.Fatal(err)
	}
	if !r2.NewerThan(r1) || r1.NewerThan(r2) || r2.NewerThan(r2) || !r1.NewerThan(nil) {
		t.Fatal("version ordering")
	}
	if r2.Device(d2).Kind != KindServer {
		t.Fatal("device lookup")
	}

	// A roster from a different account key is never newer.
	so, _ := SignRoster(testAccountOther(t), Roster{HubID: testHub, Version: 99, IssuedAt: now})
	ro, _ := VerifyRoster(so, testHub)
	if ro.NewerThan(r2) {
		t.Fatal("other account's roster accepted as newer")
	}

	if _, err := VerifyRoster(s2, "bbbbbbbbbbbbbbbbbbbbbbbbbb"); !errors.Is(err, ErrWrongHub) {
		t.Errorf("wrong hub: %v", err)
	}
	if _, err := VerifyRoster(Signed{Payload: s2.Payload, Sig: s1.Sig}, testHub); !errors.Is(err, ErrBadSignature) {
		t.Errorf("mismatched signature: %v", err)
	}
	if _, err := SignRoster(acct, Roster{HubID: testHub, Version: 0, IssuedAt: now}); err == nil {
		t.Error("version 0 accepted")
	}
	if _, err := SignRoster(acct, Roster{HubID: testHub, Version: 3, IssuedAt: now, Devices: []RosterDevice{
		{Pubkey: d1, Kind: KindDevice}, {Pubkey: d1, Kind: KindDevice},
	}}); err == nil {
		t.Error("duplicate device accepted")
	}

	empty, _ := SignRoster(acct, Roster{HubID: testHub, Version: 4, IssuedAt: now})
	if !bytes.Contains(empty.Payload, []byte(`"devices":[]`)) {
		t.Errorf("empty roster payload = %s", empty.Payload)
	}
}

// Cheap KDF parameters so tests stay fast.
var testKDF = &KDFParams{Memory: 64, Time: 1, Threads: 1}

func testIdentity() *Identity {
	id := &Identity{
		Profile:   Profile{DisplayName: "Ada", Email: "ada@example.com"},
		CreatedAt: time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC),
		Hubs: []HubMembership{{
			HubID: testHub, URL: "https://hub.example", Name: "Hub", Persona: 0,
			JoinedAt: time.Date(2026, 11, 28, 1, 0, 0, 0, time.UTC),
		}},
		Rosters: map[string]Signed{testHub: {Payload: []byte(`{}`), Sig: []byte{1, 2}}},
	}
	copy(id.MasterSecret[:], bytes.Repeat([]byte{7}, 32))
	return id
}

func TestFilePasswordMode(t *testing.T) {
	id := testIdentity()
	data, err := Seal(id, SealOptions{Password: "correct horse battery", KDF: testKDF})
	if err != nil {
		t.Fatal(err)
	}
	info, err := Inspect(data)
	if err != nil || info.Mode != ModePassword || info.KDF != *testKDF {
		t.Fatalf("inspect = %+v, %v", info, err)
	}
	got, err := Open(data, "correct horse battery", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.MasterSecret != id.MasterSecret || got.Profile.Email != id.Profile.Email || len(got.Hubs) != 1 || got.Hubs[0].HubID != testHub || !bytes.Equal(got.Rosters[testHub].Sig, []byte{1, 2}) {
		t.Fatalf("round trip = %+v", got)
	}
	if _, err := Open(data, "wrong horse battery", nil); !errors.Is(err, ErrUnlock) {
		t.Errorf("wrong password: %v", err)
	}
	if _, err := Seal(id, SealOptions{Password: "short", KDF: testKDF}); !errors.Is(err, ErrWeakPassword) {
		t.Errorf("weak password: %v", err)
	}
	if _, err := Seal(id, SealOptions{KDF: testKDF}); !errors.Is(err, ErrEmptyPassword) {
		t.Errorf("empty password: %v", err)
	}
}

func TestFileSecretMode(t *testing.T) {
	id := testIdentity()
	sk, err := NewSecretKey()
	if err != nil {
		t.Fatal(err)
	}
	data, err := Seal(id, SealOptions{Password: "pw", SecretKey: &sk, KDF: testKDF})
	if err != nil {
		t.Fatal(err)
	}
	if info, _ := Inspect(data); info.Mode != ModePasswordSecret {
		t.Fatalf("mode = %s", info.Mode)
	}
	if _, err := Open(data, "pw", nil); !errors.Is(err, ErrNeedSecretKey) {
		t.Errorf("no secret key: %v", err)
	}
	other, _ := NewSecretKey()
	if _, err := Open(data, "pw", &other); !errors.Is(err, ErrUnlock) {
		t.Errorf("wrong secret key: %v", err)
	}
	parsed, err := ParseSecretKey(strings.ToLower(sk.String()))
	if err != nil || parsed != sk {
		t.Fatalf("secret key round trip: %v", err)
	}
	got, err := Open(data, "pw", &parsed)
	if err != nil || got.MasterSecret != id.MasterSecret {
		t.Fatalf("open: %v", err)
	}
}

func TestFileTamper(t *testing.T) {
	data, err := Seal(testIdentity(), SealOptions{Password: "correct horse battery", KDF: testKDF})
	if err != nil {
		t.Fatal(err)
	}
	// Flip one bit at every position: each must be refused.
	for i := range data {
		bad := bytes.Clone(data)
		bad[i] ^= 1
		if _, err := Open(bad, "correct horse battery", nil); err == nil {
			t.Fatalf("tampered byte %d accepted", i)
		}
	}
	if _, err := Open(data[:len(data)-1], "correct horse battery", nil); err == nil {
		t.Error("truncated file accepted")
	}
	if _, err := Open([]byte("hello"), "x", nil); !errors.Is(err, ErrNotIdentityFile) {
		t.Errorf("not a file: %v", err)
	}
	newer := bytes.Clone(data)
	newer[len(fileMagic)] = fileVersion + 1
	if _, err := Open(newer, "x", nil); !errors.Is(err, ErrFileVersion) {
		t.Errorf("newer version: %v", err)
	}
}

func TestMasterSecretJSON(t *testing.T) {
	var m MasterSecret
	copy(m[:], bytes.Repeat([]byte{7}, 32))
	b, err := json.Marshal(&Identity{MasterSecret: m})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"master_secret":"`+b64(m[:])+`"`)) {
		t.Fatalf("encoded = %s", b)
	}
	var back Identity
	if err := json.Unmarshal(b, &back); err != nil || back.MasterSecret != m {
		t.Fatalf("decode: %v", err)
	}
	if err := json.Unmarshal([]byte(`{"master_secret":"AAAA"}`), &back); err == nil {
		t.Fatal("short master secret accepted")
	}
}

func TestFileRejectsHugeKDF(t *testing.T) {
	if _, err := Seal(testIdentity(), SealOptions{Password: "correct horse battery", KDF: &KDFParams{Memory: maxKDFMemory + 1, Time: 1, Threads: 1}}); err == nil {
		t.Fatal("huge memory accepted")
	}
}

// zeroReader makes salt and nonce all zeros, so the output is fixed.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// A fixed file pins the v1 format: header layout, KDF, key wrapping and AEAD.
func TestFileVector(t *testing.T) {
	id := testIdentity()
	data, err := seal(zeroReader{}, id, SealOptions{Password: "correct horse battery", KDF: testKDF})
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := "OURCHIVE-ID\x01\x00\x00\x00\x59" + `{"kdf":"argon2id","salt":"AAAAAAAAAAAAAAAAAAAAAA==","m":64,"t":1,"p":1,"mode":"password"}`
	if !bytes.HasPrefix(data, []byte(wantPrefix)) {
		t.Fatalf("header = %q", data[:min(len(data), len(wantPrefix))])
	}
	got, err := Open(data, "correct horse battery", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.MasterSecret != id.MasterSecret {
		t.Fatal("master secret round trip")
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != fileVectorSHA256 {
		t.Fatalf("file sha256 = %s", got)
	}
}

const fileVectorSHA256 = "7c7c816655e131626c8995c186ee4b099ae99a346257708c1818d57d5843582c"

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
