package winlayout

import "testing"

func TestModeFor(t *testing.T) {
	if ModeFor(false) != Compact || ModeFor(true) != Full {
		t.Fatal("compact until a hub is signed in, then full")
	}
}

func TestLayoutFitsEveryCommonScreen(t *testing.T) {
	// Work areas are in device-independent pixels: physical pixels divided by the display scale,
	// minus the taskbar.
	screens := map[string]Size{
		"1080p at 100%":          {1920, 1040},
		"1080p at 125%":          {1536, 824},
		"1080p at 150%":          {1280, 680},
		"laptop 1366x768 @ 100%": {1366, 728},
		"1440p at 200%":          {1280, 680},
		"4k at 150%":             {2560, 1360},
		"small 800x600":          {800, 560},
	}
	for name, work := range screens {
		for _, mode := range []Mode{Compact, Full} {
			l := For(mode, work)
			if l.Size.W > work.W || l.Size.H > work.H {
				t.Errorf("%s mode %d: window %+v is larger than the work area %+v", name, mode, l.Size, work)
			}
			if l.Size.W > work.W*fitPercent/100 || l.Size.H > work.H*fitPercent/100 {
				t.Errorf("%s mode %d: window %+v leaves less than %d%% margin in %+v", name, mode, l.Size, 100-fitPercent, work)
			}
			if l.Min.W > l.Size.W || l.Min.H > l.Size.H {
				t.Errorf("%s mode %d: minimum %+v is larger than the size %+v", name, mode, l.Min, l.Size)
			}
		}
	}
}

func TestCompactIsPhoneSizedAndFixed(t *testing.T) {
	l := For(Compact, Size{1920, 1040})
	if l.Size != (Size{420, 680}) || l.Resizable || l.Min != l.Size {
		t.Fatalf("compact on a large screen: %+v", l)
	}
	if l := For(Compact, Size{1280, 680}); l.Size.H != 612 || l.Size.W != 420 {
		t.Fatalf("compact should shrink to fit a short screen, got %+v", l.Size)
	}
}

func TestFullKeepsItsPreferredSizeWhenItFits(t *testing.T) {
	l := For(Full, Size{2560, 1360})
	if l.Size != (Size{1280, 820}) || l.Min != (Size{800, 560}) || !l.Resizable {
		t.Fatalf("full on a large screen: %+v", l)
	}
	if l := For(Full, Size{1280, 680}); l.Size != (Size{1152, 612}) {
		t.Fatalf("full at 150%% on 1080p: %+v", l.Size)
	}
}

func TestUnknownWorkAreaFallsBackToPreferred(t *testing.T) {
	if l := For(Full, Size{}); l.Size != (Size{1280, 820}) {
		t.Fatalf("got %+v", l.Size)
	}
	if l := For(Compact, Size{W: -1, H: 0}); l.Size != (Size{420, 680}) {
		t.Fatalf("got %+v", l.Size)
	}
}
