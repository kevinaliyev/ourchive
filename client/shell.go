package main

import (
	"crypto/rand"
	"encoding/hex"
	"log"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"
)

// wailsShell implements clientcore.Shell with Wails' native APIs.
type wailsShell struct {
	app      *application.App
	window   *application.WebviewWindow
	notifier *notifications.NotificationService
}

func (s *wailsShell) OpenExternal(url string) error {
	return s.app.Browser.OpenURL(url)
}

func (s *wailsShell) SaveFileDialog(suggestedName string) (string, error) {
	d := s.app.Dialog.SaveFile().
		SetFilename(suggestedName).
		CanCreateDirectories(true)
	if s.window != nil {
		d = d.AttachToWindow(s.window)
	}
	return d.PromptForSingleSelection()
}

func (s *wailsShell) Notify(title, body string) {
	if s.notifier == nil {
		return
	}
	id := make([]byte, 8)
	_, _ = rand.Read(id)
	if err := s.notifier.SendNotification(notifications.NotificationOptions{
		ID:    "ourchive-" + hex.EncodeToString(id),
		Title: title,
		Body:  body,
	}); err != nil {
		log.Printf("notification failed: %v", err)
	}
}

func (s *wailsShell) ShowWindow() {
	if s.window == nil {
		return
	}
	s.window.Show()
	s.window.Restore()
	s.window.Focus()
}
