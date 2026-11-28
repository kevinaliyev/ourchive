// Command ourchive is the Ourchive desktop client: the hub web UI in a native window, a tray icon,
// and (on desktop platforms) the contributor agent running in the same process.
//
// Closing the window hides it to the tray; the agent keeps contributing. Connection problems show
// up as the tray icon's colour, a notification and a Retry item instead of a console window that
// has to stay open.
package main

import (
	"context"
	"embed"
	"flag"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"

	"github.com/ourchive/ourchive/internal/agentcore"
	"github.com/ourchive/ourchive/internal/agentlog"
	"github.com/ourchive/ourchive/internal/clientcore"
)

// The web UI built with `npm run build:client` in ../web lands here.
//
//go:embed all:frontend/dist
var embedded embed.FS

// version is stamped by CI (version_release.go) or with -ldflags "-X main.version=v0.4.0".
var version = "dev"

const appID = "com.ourchive.client"

// backgroundFlag is passed by the login item so the app starts in the tray.
const backgroundFlag = "--background"

func main() {
	background := flag.Bool(strings.TrimPrefix(backgroundFlag, "--"), false, "start hidden in the system tray")
	flag.Parse()

	logPath, closeLog, err := agentlog.Init(agentlog.Options{Name: "client.log"})
	defer closeLog()
	if err != nil {
		log.Printf("warning: log file unavailable (%s): %v", logPath, err)
	}
	log.Printf("ourchive client %s starting", version)

	assets, err := fs.Sub(embedded, "frontend/dist")
	if err != nil {
		log.Fatalf("frontend assets: %v", err)
	}

	shell := &wailsShell{}
	contributor := startContributor()
	defer contributor.stop()

	core, err := clientcore.New(clientcore.Options{
		StatePath:       filepath.Join(agentcore.ConfigDir(), "client", "state.json"),
		Version:         version,
		Shell:           shell,
		Agent:           contributor.agent,
		Enroll:          contributor.enroll,
		Mobile:          isMobile,
		ContributorNote: contributor.note,
	})
	if err != nil {
		log.Fatalf("client state: %v", err)
	}

	notifier := notifications.New()
	app := application.New(application.Options{
		Name:        "Ourchive",
		Description: "Ourchive desktop client",
		Services:    []application.Service{application.NewService(notifier)},
		Assets: application.AssetOptions{
			Handler:        application.AssetFileServerFS(assets),
			Middleware:     core.Middleware,
			DisableLogging: true,
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: false,
		},
		Windows: application.WindowsOptions{
			DisableQuitOnLastWindowClosed: true,
		},
		Linux: application.LinuxOptions{
			DisableQuitOnLastWindowClosed: true,
			ProgramName:                   "ourchive",
			ApplicationID:                 appID,
		},
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: appID,
			// Windows and Linux start a second process for ourchive:// links and plain relaunches;
			// forward the link and surface the running window instead.
			OnSecondInstanceLaunch: func(data application.SecondInstanceData) {
				handled := false
				for _, arg := range data.Args {
					if isDeepLink(arg) {
						handleDeepLink(core, shell, arg)
						handled = true
					}
				}
				if !handled {
					shell.ShowWindow()
				}
			},
		},
		OnShutdown: func() {
			contributor.stop()
			core.Close()
		},
	})
	shell.app = app
	shell.notifier = notifier

	window := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:             "main",
		Title:            "Ourchive",
		Width:            1280,
		Height:           820,
		MinWidth:         420,
		MinHeight:        560,
		URL:              "/",
		Hidden:           *background,
		BackgroundColour: application.NewRGB(17, 17, 19),
	})
	shell.window = window

	if !isMobile {
		// Closing the window keeps the app (and the contributor agent) running in the tray.
		window.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
			window.Hide()
			e.Cancel()
		})
		app.Event.OnApplicationEvent(events.Mac.ApplicationShouldHandleReopen, func(*application.ApplicationEvent) {
			shell.ShowWindow()
		})
		newTray(app, core, shell, contributor).start()
	}

	// macOS delivers ourchive:// links as an event; Windows and Linux pass them as the first
	// argument of a cold start, which Wails also reports through this event.
	app.Event.OnApplicationEvent(events.Common.ApplicationLaunchedWithUrl, func(e *application.ApplicationEvent) {
		if u := e.Context().URL(); isDeepLink(u) {
			handleDeepLink(core, shell, u)
		}
	})
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		go func() {
			if ok, err := notifier.CheckNotificationAuthorization(); err == nil && !ok {
				_, _ = notifier.RequestNotificationAuthorization()
			}
		}()
	})

	contributor.run(context.Background())

	if err := app.Run(); err != nil {
		log.Printf("app exited with error: %v", err)
		os.Exit(1)
	}
}

func isDeepLink(s string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(s)), clientcore.DeepLinkScheme+"://")
}

func handleDeepLink(core *clientcore.Core, shell *wailsShell, link string) {
	if err := core.HandleDeepLink(link); err != nil {
		log.Printf("deep link rejected: %v", err)
		shell.Notify("Ourchive", err.Error())
	}
}
