package main

import (
	"log"
	"path/filepath"
	"sync"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/ourchive/ourchive/internal/agentlog"
	"github.com/ourchive/ourchive/internal/agentstatus"
	"github.com/ourchive/ourchive/internal/clientcore"

	"github.com/ourchive/ourchive/client/internal/trayui"
)

// tray is the always-present control surface: it shows whether this computer is contributing,
// and offers Open, Retry, Start at login and Quit without needing the window or a console.
type tray struct {
	app         *application.App
	core        *clientcore.Core
	shell       *wailsShell
	contributor *contributor

	systray   *application.SystemTray
	statusRow *application.MenuItem
	retryRow  *application.MenuItem
	loginRow  *application.MenuItem
	menu      *application.Menu

	mu          sync.Mutex
	lastSummary agentstatus.Summary
}

func newTray(app *application.App, core *clientcore.Core, shell *wailsShell, c *contributor) *tray {
	return &tray{app: app, core: core, shell: shell, contributor: c, lastSummary: agentstatus.SummaryNone}
}

func (t *tray) start() {
	t.systray = t.app.SystemTray.New()
	t.menu = t.app.Menu.New()

	t.menu.Add("Open Ourchive").OnClick(func(*application.Context) { t.shell.ShowWindow() })
	t.menu.AddSeparator()
	t.statusRow = t.menu.Add("Not contributing").SetEnabled(false)
	t.retryRow = t.menu.Add("Retry connections").OnClick(func(*application.Context) {
		t.contributor.retryAll()
	})
	t.menu.AddSeparator()
	enabled, _ := t.app.Autostart.IsEnabled()
	t.loginRow = t.menu.AddCheckbox("Start at login", enabled).OnClick(func(ctx *application.Context) {
		t.setStartAtLogin(ctx.ClickedMenuItem().Checked())
	})
	t.menu.Add("Show log file").OnClick(func(*application.Context) {
		if err := t.app.Browser.OpenFile(filepath.Dir(agentlog.DefaultPath("client.log"))); err != nil {
			log.Printf("open logs folder: %v", err)
		}
	})
	t.menu.AddSeparator()
	t.menu.Add("Quit Ourchive").OnClick(func(*application.Context) { t.app.Quit() })

	t.systray.SetMenu(t.menu)
	t.systray.OnClick(func() { t.shell.ShowWindow() })
	t.render(t.core.Snapshot())

	updates, _ := t.core.Subscribe()
	go func() {
		for range updates {
			t.render(t.core.Snapshot())
		}
	}()
}

func (t *tray) setStartAtLogin(on bool) {
	var err error
	if on {
		err = t.app.Autostart.EnableWithOptions(application.AutostartOptions{Arguments: []string{backgroundFlag}})
	} else {
		err = t.app.Autostart.Disable()
	}
	if err != nil {
		log.Printf("start at login: %v", err)
		t.shell.Notify("Ourchive", "Could not change Start at login: "+err.Error())
		t.loginRow.SetChecked(!on)
		t.menu.Update()
	}
}

func (t *tray) render(st clientcore.State) {
	summary := st.AgentSummary
	label, tooltip := trayui.Describe(st)

	t.mu.Lock()
	prev := t.lastSummary
	t.lastSummary = summary
	t.mu.Unlock()

	t.systray.SetIcon(trayui.StatusIcon(summary))
	t.systray.SetTooltip(tooltip)
	t.statusRow.SetLabel(label)
	t.retryRow.SetEnabled(summary == agentstatus.SummaryFailed || summary == agentstatus.SummaryDegraded)
	t.menu.Update()

	// Tell the user once when everything has stopped retrying, and once when it recovers.
	switch {
	case summary == agentstatus.SummaryFailed && prev != agentstatus.SummaryFailed:
		t.shell.Notify("Ourchive can't reach your hubs", "Contributing is paused. Choose Retry connections in the tray when you're back online.")
	case prev == agentstatus.SummaryFailed && summary == agentstatus.SummaryOK:
		t.shell.Notify("Ourchive reconnected", "This computer is contributing again.")
	}
}
