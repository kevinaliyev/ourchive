package trayui

import (
	"bytes"
	"image/png"
	"strings"
	"testing"

	"github.com/ourchive/ourchive/internal/agentstatus"
	"github.com/ourchive/ourchive/internal/clientcore"
)

func TestStatusIconsAreDistinctPNGs(t *testing.T) {
	seen := map[string]agentstatus.Summary{}
	for _, s := range []agentstatus.Summary{agentstatus.SummaryNone, agentstatus.SummaryOK, agentstatus.SummaryDegraded, agentstatus.SummaryFailed} {
		b := StatusIcon(s)
		img, err := png.Decode(bytes.NewReader(b))
		if err != nil || img.Bounds().Dx() != 64 {
			t.Fatalf("%s: invalid icon (%v)", s, err)
		}
		if _, _, _, a := img.At(32, 32).RGBA(); a == 0 {
			t.Fatalf("%s: centre dot missing", s)
		}
		if _, _, _, a := img.At(0, 0).RGBA(); a != 0 {
			t.Fatalf("%s: corners should be transparent", s)
		}
		if prev, dup := seen[string(b)]; dup {
			t.Fatalf("%s and %s share an icon", s, prev)
		}
		seen[string(b)] = s
	}
}

func TestDescribe(t *testing.T) {
	st := clientcore.State{Platform: clientcore.PlatformInfo{Contributor: true}, Agent: []agentstatus.HubStatus{
		{HubURL: "https://a.example", State: agentstatus.StateConnected},
		{HubURL: "https://b.example", State: agentstatus.StateRetrying, Failures: 2, MaxRetries: 5},
	}}
	label, tip := Describe(st)
	if label != "Contributing to 1 of 2 hubs" || !strings.Contains(tip, "b.example: reconnecting (2/5)") {
		t.Fatalf("unexpected describe output %q / %q", label, tip)
	}
	if label, _ := Describe(clientcore.State{}); label != "Not contributing on this computer" {
		t.Fatalf("unexpected label for non-contributor build: %q", label)
	}
}
