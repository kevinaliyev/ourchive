package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/ourchive/ourchive/internal/agentcore"
	"github.com/ourchive/ourchive/internal/agentlog"
	"github.com/ourchive/ourchive/internal/agentstatus"
	"github.com/ourchive/ourchive/internal/compute"
	"github.com/ourchive/ourchive/internal/service"
	"github.com/ourchive/ourchive/internal/updater"
	"github.com/ourchive/ourchive/pkg/protocol"
)

var knownHWEncoders = []string{
	"h264_nvenc", "hevc_nvenc", "av1_nvenc",
	"h264_amf", "hevc_amf", "av1_amf",
	"h264_qsv", "hevc_qsv", "av1_qsv",
	"h264_videotoolbox", "hevc_videotoolbox",
}

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(val string) error {
	if strings.TrimSpace(val) != "" {
		*m = append(*m, strings.TrimSpace(val))
	}
	return nil
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "version":
		fmt.Printf("ourchive-agent %s (%s/%s)\n", updater.Version, runtime.GOOS, runtime.GOARCH)
	case "probe":
		runProbe()
	case "list":
		runListHubs()
	case "enroll":
		runEnroll(os.Args[2:])
	case "run":
		runDaemon(os.Args[2:])
	case "update":
		runUpdate(os.Args[2:])
	case "service":
		runService(os.Args[2:])
	case "logs":
		runLogs(os.Args[2:])
	default:
		printUsage()
		os.Exit(1)
	}
}

type AgentGlobalSettings struct {
	AutoUpdate         bool   `json:"auto_update"`
	UpdateURL          string `json:"update_url,omitempty"`
	CheckIntervalHours int    `json:"check_interval_hours,omitempty"`
}

func globalSettingsPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".config", "ourchive", "agent-settings.json")
	}
	return filepath.Join(home, ".config", "ourchive", "agent-settings.json")
}

func loadGlobalSettings() AgentGlobalSettings {
	gs := AgentGlobalSettings{CheckIntervalHours: 6}
	data, err := os.ReadFile(globalSettingsPath())
	if err == nil {
		_ = json.Unmarshal(data, &gs)
		if gs.CheckIntervalHours <= 0 {
			gs.CheckIntervalHours = 6
		}
	}
	return gs
}

func saveGlobalSettings(gs AgentGlobalSettings) error {
	p := globalSettingsPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(gs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

func runUpdate(args []string) {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	checkOnly := fs.Bool("check", false, "Only check if an update is available")
	urlOverride := fs.String("url", "", "Manifest or GitHub Releases API URL")
	enableAuto := fs.Bool("enable-auto", false, "Opt into silent background auto-updates")
	disableAuto := fs.Bool("disable-auto", false, "Disable silent background auto-updates")
	_ = fs.Parse(args)

	gs := loadGlobalSettings()
	if strings.TrimSpace(*urlOverride) != "" {
		gs.UpdateURL = strings.TrimSpace(*urlOverride)
	}
	if *enableAuto {
		gs.AutoUpdate = true
		_ = saveGlobalSettings(gs)
		fmt.Printf("Silent auto-updates enabled (settings saved to %s)\n", globalSettingsPath())
	} else if *disableAuto {
		gs.AutoUpdate = false
		_ = saveGlobalSettings(gs)
		fmt.Printf("Silent auto-updates disabled (settings saved to %s)\n", globalSettingsPath())
	}

	if gs.UpdateURL == "" {
		log.Fatal("no update_url configured; pass --url https://api.github.com/repos/<owner>/<repo>/releases/latest")
	}
	if strings.TrimSpace(*urlOverride) != "" {
		_ = saveGlobalSettings(gs)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := updater.CheckForUpdate(ctx, gs.UpdateURL, updater.Version)
	if err != nil {
		log.Fatalf("update check failed: %v", err)
	}
	if !res.UpdateAvail {
		fmt.Printf("ourchive-agent is up to date (%s)\n", res.CurrentVersion)
		return
	}
	fmt.Printf("Update available: %s -> %s\n", res.CurrentVersion, res.LatestVersion)
	if *checkOnly {
		return
	}

	exePath, err := os.Executable()
	if err != nil {
		log.Fatalf("locate executable: %v", err)
	}
	if err := updater.ApplyUpdate(ctx, res.Asset, exePath); err != nil {
		log.Fatalf("apply update: %v", err)
	}
	fmt.Printf("Updated to %s. Restart ourchive-agent to run the new version.\n", res.LatestVersion)
}

func runService(args []string) {
	if len(args) < 1 {
		log.Fatal("usage: ourchive-agent service <install|uninstall>")
	}
	switch args[0] {
	case "install":
		exePath, err := os.Executable()
		if err != nil {
			log.Fatalf("locate executable: %v", err)
		}
		msg, err := service.Install(exePath)
		if err != nil {
			log.Fatalf("service install: %v", err)
		}
		fmt.Println(msg)
	case "uninstall":
		msg, err := service.Uninstall()
		if err != nil {
			log.Fatalf("service uninstall: %v", err)
		}
		fmt.Println(msg)
	default:
		log.Fatal("usage: ourchive-agent service <install|uninstall>")
	}
}

func runListHubs() {
	paths := agentcore.DiscoverHubConfigs()
	if len(paths) == 0 {
		fmt.Printf("No enrolled hubs found in %s\n", agentcore.HubsDir())
		return
	}
	fmt.Printf("Enrolled hubs (%s):\n", agentcore.HubsDir())
	for _, p := range paths {
		cfg, err := agentcore.LoadConfig(p)
		if err != nil {
			fmt.Printf("  - %s (invalid: %v)\n", filepath.Base(p), err)
			continue
		}
		fmt.Printf("  - %s -> %s (node: %s, mounts: %d)\n", filepath.Base(p), cfg.HubURL, cfg.DisplayName, len(cfg.Mounts))
	}
}

func runProbe() {
	hostname, _ := os.Hostname()
	manifest := protocol.NodeManifest{
		NodeID:      strings.ToLower(hostname),
		DisplayName: hostname,
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		Status:      protocol.NodeStatusOnline,
		Compute:     compute.ProbeCompute(),
		UpdatedAt:   time.Now().UTC(),
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(manifest)
}

func runEnroll(args []string) {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	hubURL := fs.String("hub", "http://localhost:8080", "Ourchive Hub URL")
	token := fs.String("token", "", "One-time enrollment token from Contribution settings")
	name := fs.String("name", "", "Display name for this machine (defaults to hostname)")
	cfgPath := fs.String("config", "", "Path to save agent configuration (default: $HOME/.config/ourchive/hubs/<hub>.json)")
	readOnly := fs.Bool("read-only", false, "Never accept remote file uploads, mkdir, or deletes on this machine")
	var allowDirs multiFlag
	fs.Var(&allowDirs, "allow-dir", "Optional allowed base directory whitelist (repeatable)")
	_ = fs.Parse(args)

	if *token == "" {
		log.Fatal("--token is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, err := agentcore.Enroll(ctx, agentcore.EnrollOptions{
		HubURL:     *hubURL,
		Token:      *token,
		Name:       *name,
		ConfigPath: *cfgPath,
		ReadOnly:   *readOnly,
		AllowDirs:  allowDirs,
	})
	if err != nil {
		log.Fatalf("%v", err)
	}

	fmt.Printf("Enrolled machine %q (node_id: %s)\n", res.DisplayName, res.NodeID)
	fmt.Printf("Config saved to %s\n", res.ConfigPath)
	fmt.Printf("Start the headless agent (connects to all enrolled hubs) with:\n  ourchive-agent run\n")
}

func runDaemon(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("config", "", "Optional single config file (default: connect to all hubs in $HOME/.config/ourchive/hubs/)")
	forceReadOnly := fs.Bool("read-only", false, "Force read-only mode across all hubs (blocks all remote writes/deletes)")
	maxRetries := fs.Int("max-retries", 5, "Maximum consecutive failed connection attempts per hub before stopping (0 = unlimited)")
	logFile := fs.String("log-file", "", "Optional log file path (default: OS user logs folder)")
	_ = fs.Parse(args)

	logPath, closeLog, logErr := agentlog.Init(agentlog.Options{Path: *logFile, Name: "agent.log", Stderr: true})
	defer closeLog()
	if logErr != nil {
		log.Printf("warning: failed to initialize log file (%s): %v", logPath, logErr)
		logPath = ""
	} else {
		log.Printf("ourchive-agent %s starting (log file: %s)", updater.Version, logPath)
	}

	var configPaths []string
	if strings.TrimSpace(*cfgPath) != "" {
		configPaths = []string{strings.TrimSpace(*cfgPath)}
	} else {
		configPaths = agentcore.DiscoverHubConfigs()
		if len(configPaths) == 0 {
			// Fallback to legacy ./agent.json if present.
			if _, err := os.Stat("agent.json"); err == nil {
				configPaths = []string{"agent.json"}
			}
		}
	}
	if len(configPaths) == 0 {
		log.Fatalf("no enrolled hubs found in %s (run `ourchive-agent enroll --hub <url> --token <tok>` first)", agentcore.HubsDir())
	}

	runner := agentcore.New(agentcore.Options{
		ConfigPaths:   configPaths,
		ForceReadOnly: *forceReadOnly,
		MaxRetries:    *maxRetries,
	})
	if len(runner.Hubs()) == 0 {
		log.Fatalf("no valid hub configurations found in %s", agentcore.HubsDir())
	}

	lock, err := agentcore.AcquireInstanceLockWait("ourchive-agent", 15*time.Second)
	if err != nil {
		log.Fatalf("%v; if the Ourchive desktop app is running it already contributes this computer, so quit it or stop this service (`ourchive-agent service uninstall`)", err)
	}
	defer lock.Release()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if exePath, err := os.Executable(); err == nil {
		updater.CleanupOldBinary(exePath)
		gs := loadGlobalSettings()
		if gs.AutoUpdate && gs.UpdateURL != "" {
			go runAutoUpdateLoop(ctx, exePath, gs, stop)
		}
	}

	runDone := make(chan struct{})
	go func() {
		_ = runner.Run(ctx)
		close(runDone)
	}()

	updates, unsubscribe := runner.Subscribe()
	defer unsubscribe()
	for {
		select {
		case <-runDone:
			log.Println("ourchive agent stopped")
			return
		case snap := <-updates:
			if agentstatus.Summarize(snap) != agentstatus.SummaryFailed {
				continue
			}
			log.Printf("ERROR: all enrolled hubs stopped after reaching max retries (%d)", *maxRetries)
			if isInteractiveTerminal(os.Stdin) {
				if promptAfterFailure(logPath, os.Stdin, os.Stdout) {
					runner.RetryAll()
					continue
				}
			} else if logPath != "" {
				log.Printf("exiting after connection failures; inspect logs at %s", logPath)
			}
			stop()
			<-runDone
			os.Exit(1)
		}
	}
}

func runAutoUpdateLoop(ctx context.Context, exePath string, gs AgentGlobalSettings, stop context.CancelFunc) {
	interval := time.Duration(gs.CheckIntervalHours) * time.Hour
	if interval < time.Hour {
		interval = time.Hour
	}
	check := func() {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		res, err := updater.CheckForUpdate(cctx, gs.UpdateURL, updater.Version)
		if err != nil || !res.UpdateAvail {
			return
		}
		log.Printf("auto-update: downloading %s -> %s...", res.CurrentVersion, res.LatestVersion)
		if err := updater.ApplyUpdate(cctx, res.Asset, exePath); err != nil {
			log.Printf("auto-update failed: %v", err)
			return
		}
		log.Printf("auto-update: upgraded to %s; spawning updated agent and exiting...", res.LatestVersion)
		cmd := exec.Command(exePath, os.Args[1:]...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			log.Printf("auto-update restart failed: %v", err)
			return
		}
		stop()
	}

	// Quiet initial check 30s after boot, then periodic.
	select {
	case <-ctx.Done():
		return
	case <-time.After(30 * time.Second):
		check()
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check()
		}
	}
}

func printUsage() {
	fmt.Println("Usage: ourchive-agent <version|probe|list|enroll|run|logs|update|service>")
}
