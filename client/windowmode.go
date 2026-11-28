package main

import (
	"log"
	"sync"

	"github.com/ourchive/ourchive/client/internal/winlayout"
	"github.com/ourchive/ourchive/internal/clientcore"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// windowController sizes the one app window to its purpose: a compact, phone-sized window while
// nobody is signed in (so the login isn't lost in a huge frame), and the full window once a hub is
// signed in. Sizes come from winlayout and are always clamped to the screen's usable area, in
// device-independent pixels, so they are right at any display scaling. Wails makes the process
// per-monitor DPI aware itself.
type windowController struct {
	window *application.WebviewWindow

	mu   sync.Mutex
	mode winlayout.Mode
	set  bool
}

// modeFor is the window the current state calls for.
func modeFor(st clientcore.State) winlayout.Mode {
	for _, h := range st.Hubs {
		if h.SignedIn {
			return winlayout.ModeFor(true)
		}
	}
	return winlayout.ModeFor(false)
}

// apply resizes and recentres the window when the mode changes; it does nothing if the mode is the
// one already applied, so a user's own resizing of the full window is left alone. Call it on the
// main thread (application.InvokeSync).
func (c *windowController) apply(mode winlayout.Mode) {
	c.mu.Lock()
	if c.set && c.mode == mode {
		c.mu.Unlock()
		return
	}
	c.mode, c.set = mode, true
	c.mu.Unlock()

	var work winlayout.Size
	if screen, err := c.window.GetScreen(); err == nil && screen != nil {
		work = winlayout.Size{W: screen.WorkArea.Width, H: screen.WorkArea.Height}
	}
	l := winlayout.For(mode, work)
	log.Printf("window: mode %d, %dx%d (work area %dx%d)", mode, l.Size.W, l.Size.H, work.W, work.H)

	if c.window.IsMaximised() {
		c.window.Restore()
	}
	// Drop the old minimum first, or shrinking from the full window would be held at its size.
	c.window.SetMinSize(0, 0)
	c.window.SetSize(l.Size.W, l.Size.H)
	c.window.SetMinSize(l.Min.W, l.Min.H)
	c.window.SetResizable(l.Resizable)
	c.window.Center()
}

// follow applies the window for the current state, shows the window unless the app was started in
// the background, and then keeps the window matched to sign-in changes. Run it from the
// ApplicationStarted event.
func (c *windowController) follow(core *clientcore.Core, shell *wailsShell, startHidden bool) {
	updates, _ := core.Subscribe()
	go func() {
		application.InvokeSync(func() { c.apply(modeFor(core.Snapshot())) })
		if !startHidden {
			shell.ShowWindow()
		}
		for range updates {
			mode := modeFor(core.Snapshot())
			application.InvokeSync(func() { c.apply(mode) })
		}
	}()
}
