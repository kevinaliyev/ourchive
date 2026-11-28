// Package identity implements the Ourchive identity primitives: the recovery
// phrase, the master secret and per-hub key derivations, the encrypted
// identity file, device certificates and signed device lists (rosters).
//
// It has no UI, network or database code. See
// docs/identity-and-devices-design.md §5 for the design.
package identity

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	_ "embed"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// The BIP39 English wordlist, from github.com/bitcoin/bips
// (bip-0039/english.txt, SHA-256 2f5eed53…24dbda). TestWordlistHash pins it.
//
//go:embed wordlist_english.txt
var wordlistText string

var (
	wordlist  []string
	wordIndex map[string]int
)

func init() {
	wordlist = strings.Fields(strings.ReplaceAll(wordlistText, "\r", ""))
	if len(wordlist) != 2048 {
		panic(fmt.Sprintf("identity: wordlist has %d words, want 2048", len(wordlist)))
	}
	wordIndex = make(map[string]int, len(wordlist))
	for i, w := range wordlist {
		wordIndex[w] = i
	}
}

// PhraseWords is the number of words in an Ourchive recovery phrase.
const PhraseWords = 24

// entropyBytes is the entropy carried by a 24-word phrase (256 bits).
const entropyBytes = 32

var (
	ErrPhraseLength   = errors.New("identity: recovery phrase must have 24 words")
	ErrPhraseWord     = errors.New("identity: recovery phrase has a word that isn't in the list")
	ErrPhraseChecksum = errors.New("identity: recovery phrase checksum doesn't match (a word is wrong or out of order)")
)

// Phrase is a validated 24-word BIP39 recovery phrase.
type Phrase struct {
	words []string
}

// NewPhrase makes a phrase from 256 bits of fresh randomness.
func NewPhrase() (Phrase, error) {
	ent := make([]byte, entropyBytes)
	if _, err := rand.Read(ent); err != nil {
		return Phrase{}, err
	}
	return PhraseFromEntropy(ent)
}

// PhraseFromEntropy encodes 32 bytes of entropy as a 24-word phrase.
func PhraseFromEntropy(ent []byte) (Phrase, error) {
	if len(ent) != entropyBytes {
		return Phrase{}, fmt.Errorf("identity: entropy must be %d bytes, got %d", entropyBytes, len(ent))
	}
	return Phrase{words: entropyToWords(ent)}, nil
}

// entropyToWords is BIP39 encoding for any entropy length that is a multiple
// of 4 bytes: append the first len/32 bits of SHA-256, split into 11-bit words.
func entropyToWords(ent []byte) []string {
	sum := sha256.Sum256(ent)
	csBits := len(ent) * 8 / 32
	totalBits := len(ent)*8 + csBits
	bit := func(i int) int {
		if i < len(ent)*8 {
			return int(ent[i/8]>>(7-uint(i%8))) & 1
		}
		j := i - len(ent)*8
		return int(sum[j/8]>>(7-uint(j%8))) & 1
	}
	words := make([]string, 0, totalBits/11)
	for w := 0; w < totalBits/11; w++ {
		idx := 0
		for b := 0; b < 11; b++ {
			idx = idx<<1 | bit(w*11+b)
		}
		words = append(words, wordlist[idx])
	}
	return words
}

// ParsePhrase checks a phrase typed or pasted by the user. Words may be
// separated by any whitespace and are matched case-insensitively.
func ParsePhrase(s string) (Phrase, error) {
	words := strings.Fields(strings.ToLower(norm.NFKD.String(s)))
	if len(words) != PhraseWords {
		return Phrase{}, ErrPhraseLength
	}
	if _, err := wordsToEntropy(words); err != nil {
		return Phrase{}, err
	}
	return Phrase{words: words}, nil
}

func wordsToEntropy(words []string) ([]byte, error) {
	if len(words) == 0 || len(words)%3 != 0 {
		return nil, ErrPhraseLength
	}
	totalBits := len(words) * 11
	csBits := totalBits / 33
	entBits := totalBits - csBits
	bits := make([]byte, totalBits)
	for i, w := range words {
		idx, ok := wordIndex[w]
		if !ok {
			return nil, ErrPhraseWord
		}
		for b := 0; b < 11; b++ {
			bits[i*11+b] = byte(idx>>(10-uint(b))) & 1
		}
	}
	ent := make([]byte, entBits/8)
	for i := 0; i < entBits; i++ {
		ent[i/8] |= bits[i] << (7 - uint(i%8))
	}
	sum := sha256.Sum256(ent)
	for i := 0; i < csBits; i++ {
		if bits[entBits+i] != (sum[i/8]>>(7-uint(i%8)))&1 {
			return nil, ErrPhraseChecksum
		}
	}
	return ent, nil
}

// Words returns a copy of the phrase's words, in order.
func (p Phrase) Words() []string { return append([]string(nil), p.words...) }

// String returns the words separated by single spaces.
func (p Phrase) String() string { return strings.Join(p.words, " ") }

// IsZero reports whether p is the zero Phrase.
func (p Phrase) IsZero() bool { return len(p.words) == 0 }

// Entropy returns the 32 bytes the phrase encodes.
func (p Phrase) Entropy() []byte {
	ent, err := wordsToEntropy(p.words)
	if err != nil {
		panic("identity: invalid Phrase value") // only reachable through a zero Phrase
	}
	return ent
}

// Seed is the BIP39 seed: PBKDF2-HMAC-SHA512 over the NFKD phrase, salted
// with "mnemonic" plus the NFKD passphrase, 2048 rounds, 64 bytes.
func (p Phrase) Seed(passphrase string) []byte {
	return bip39Seed(p.String(), passphrase)
}

func bip39Seed(mnemonic, passphrase string) []byte {
	m := norm.NFKD.String(mnemonic)
	salt := "mnemonic" + norm.NFKD.String(passphrase)
	seed, err := pbkdf2.Key(sha512.New, m, []byte(salt), 2048, 64)
	if err != nil {
		panic(err) // fixed, valid parameters
	}
	return seed
}

// MasterSecret derives the identity's master secret from the phrase:
// HKDF-SHA256(BIP39 seed, info "ourchive/v1/master"), 32 bytes.
func (p Phrase) MasterSecret(passphrase string) MasterSecret {
	seed := p.Seed(passphrase)
	defer clear(seed)
	return masterFromSeed(seed)
}
