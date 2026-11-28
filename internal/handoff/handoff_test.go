package handoff

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk" // RFC 7636 appendix B

func TestChallengeMatchesRFC7636Example(t *testing.T) {
	if got, want := Challenge(verifier), "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"; got != want {
		t.Fatalf("Challenge = %q, want %q", got, want)
	}
	if !ValidChallenge(Challenge(verifier)) {
		t.Fatalf("expected computed challenge to validate")
	}
	for _, bad := range []string{"", "short", strings.Repeat("a", 44), strings.Repeat("!", 43)} {
		if ValidChallenge(bad) {
			t.Fatalf("ValidChallenge(%q) should be false", bad)
		}
	}
}

func TestIssueRedeemIsOneTime(t *testing.T) {
	s := NewStore(time.Minute)
	code, err := s.Issue("user-1", Challenge(verifier))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if s.Pending() != 1 {
		t.Fatalf("expected one pending code")
	}
	uid, err := s.Redeem(code, verifier)
	if err != nil || uid != "user-1" {
		t.Fatalf("Redeem = %q, %v", uid, err)
	}
	if _, err := s.Redeem(code, verifier); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("second redeem should fail with ErrInvalidCode, got %v", err)
	}
}

func TestWrongVerifierBurnsCode(t *testing.T) {
	s := NewStore(time.Minute)
	code, _ := s.Issue("user-1", Challenge(verifier))
	other := strings.Repeat("x", 43)
	if _, err := s.Redeem(code, other); !errors.Is(err, ErrVerifierMismatch) {
		t.Fatalf("expected ErrVerifierMismatch, got %v", err)
	}
	// The real verifier no longer works either: an interceptor gets one guess and burns the code.
	if _, err := s.Redeem(code, verifier); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("expected burned code, got %v", err)
	}
}

func TestExpiry(t *testing.T) {
	s := NewStore(time.Minute)
	now := time.Now()
	s.now = func() time.Time { return now }
	code, _ := s.Issue("user-1", Challenge(verifier))
	now = now.Add(2 * time.Minute)
	if _, err := s.Redeem(code, verifier); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("expected expired code to be rejected, got %v", err)
	}
	if s.Pending() != 0 {
		t.Fatalf("expected pruned store")
	}
}

func TestIssueRejectsBadChallenge(t *testing.T) {
	if _, err := NewStore(0).Issue("user-1", "plain-text-verifier"); !errors.Is(err, ErrInvalidChallenge) {
		t.Fatalf("expected ErrInvalidChallenge, got %v", err)
	}
}

func TestValidState(t *testing.T) {
	if !ValidState("abcdefghijklmnop") || ValidState("short") || ValidState("has spaces in it!!") || ValidState(strings.Repeat("a", 129)) {
		t.Fatalf("ValidState boundaries wrong")
	}
}
