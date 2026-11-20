package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const taskName = "OurchiveAgent"

// Install registers the agent binary to start silently on user login (no admin/UAC required).
func Install(exePath string) (string, error) {
	absExe, err := filepath.Abs(exePath)
	if err != nil {
		return "", err
	}

	switch runtime.GOOS {
	case "windows":
		// User-level Scheduled Task on logon; no admin privileges needed.
		tr := fmt.Sprintf(`"%s" run`, absExe)
		cmd := exec.Command("schtasks", "/Create", "/F", "/SC", "ONLOGON", "/TN", taskName, "/TR", tr)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("register scheduled task: %w (%s)", err, strings.TrimSpace(string(out)))
		}
		return fmt.Sprintf("Installed Windows logon task %q -> %s", taskName, absExe), nil

	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir := filepath.Join(home, "Library", "LaunchAgents")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		plistPath := filepath.Join(dir, "com.ourchive.agent.plist")
		plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.ourchive.agent</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>run</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
</dict>
</plist>
`, absExe)
		if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
			return "", err
		}
		_ = exec.Command("launchctl", "unload", plistPath).Run()
		_ = exec.Command("launchctl", "load", plistPath).Run()
		return fmt.Sprintf("Installed macOS LaunchAgent at %s", plistPath), nil

	default: // linux
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir := filepath.Join(home, ".config", "systemd", "user")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		unitPath := filepath.Join(dir, "ourchive-agent.service")
		unit := fmt.Sprintf(`[Unit]
Description=Ourchive Share & Compute Agent
After=network-online.target

[Service]
ExecStart=%s run
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
`, absExe)
		if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
			return "", err
		}
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
		_ = exec.Command("systemctl", "--user", "enable", "--now", "ourchive-agent.service").Run()
		return fmt.Sprintf("Installed systemd user service at %s", unitPath), nil
	}
}

// Uninstall removes the user-level auto-start registration.
func Uninstall() (string, error) {
	switch runtime.GOOS {
	case "windows":
		cmd := exec.Command("schtasks", "/Delete", "/F", "/TN", taskName)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("remove scheduled task: %w (%s)", err, strings.TrimSpace(string(out)))
		}
		return fmt.Sprintf("Removed Windows logon task %q", taskName), nil

	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		plistPath := filepath.Join(home, "Library", "LaunchAgents", "com.ourchive.agent.plist")
		_ = exec.Command("launchctl", "unload", plistPath).Run()
		_ = os.Remove(plistPath)
		return fmt.Sprintf("Removed macOS LaunchAgent %s", plistPath), nil

	default:
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		unitPath := filepath.Join(home, ".config", "systemd", "user", "ourchive-agent.service")
		_ = exec.Command("systemctl", "--user", "disable", "--now", "ourchive-agent.service").Run()
		_ = os.Remove(unitPath)
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
		return fmt.Sprintf("Removed systemd user service %s", unitPath), nil
	}
}
