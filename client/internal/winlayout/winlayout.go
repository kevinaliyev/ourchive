// Package winlayout decides how big the app window should be. It is free of GUI code so the sizing
// rules can be tested anywhere.
//
// All sizes are device-independent pixels (the unit Wails windows and screen work areas use), so
// the same numbers work at 100%, 150% or 200% display scaling.
package winlayout

// Mode is the window's purpose.
type Mode int

const (
	// Compact is the sign-in window: phone-sized, fixed, so a login form isn't lost in a huge frame.
	Compact Mode = iota
	// Full is the working window shown once a hub is signed in.
	Full
)

// Size is a width and height in device-independent pixels.
type Size struct{ W, H int }

// Layout is what to apply to the window.
type Layout struct {
	Size      Size
	Min       Size
	Resizable bool
}

var (
	compactPreferred = Size{W: 420, H: 680}
	fullPreferred    = Size{W: 1280, H: 820}
	fullMin          = Size{W: 800, H: 560}
)

// fitPercent is how much of the screen's usable area a window may take, leaving room for the
// taskbar, title bar and some desktop around it.
const fitPercent = 90

// ModeFor picks the window for the current sign-in state: compact until some hub is signed in.
// Once the full window is up it stays that way while any hub remains signed in, so switching to a
// hub that still needs a password doesn't shrink the window the user is working in.
func ModeFor(anySignedIn bool) Mode {
	if anySignedIn {
		return Full
	}
	return Compact
}

// For returns the layout for mode on a screen whose usable area (work area) is work. An unknown
// work area (zero or negative) gives the preferred sizes.
func For(mode Mode, work Size) Layout {
	if mode == Compact {
		size := clamp(compactPreferred, work)
		return Layout{Size: size, Min: size, Resizable: false}
	}
	size := clamp(fullPreferred, work)
	return Layout{Size: size, Min: Size{W: min(fullMin.W, size.W), H: min(fullMin.H, size.H)}, Resizable: true}
}

func clamp(pref, work Size) Size {
	out := pref
	if work.W > 0 {
		out.W = min(out.W, work.W*fitPercent/100)
	}
	if work.H > 0 {
		out.H = min(out.H, work.H*fitPercent/100)
	}
	return out
}
