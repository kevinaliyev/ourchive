package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"

	"github.com/ourchive/ourchive/internal/compute"
	"github.com/ourchive/ourchive/internal/service"
	"github.com/ourchive/ourchive/internal/tunnel"
	"github.com/ourchive/ourchive/internal/updater"
	"github.com/ourchive/ourchive/internal/vfs"
	"github.com/ourchive/ourchive/pkg/protocol"
)

var knownHWEncoders = []string{
	"h264_nvenc", "hevc_nvenc", "av1_nvenc",
	"h264_amf", "hevc_amf", "av1_amf",
	"h264_qsv", "hevc_qsv", "av1_qsv",
	"h264_videotoolbox", "hevc_videotoolbox",
}

// AgentConfig is persisted locally per Hub under $HOME/.config/ourchive/hubs/<hub>.json.
type AgentConfig struct {
	HubURL            string                 `json:"hub_url"`
	NodeID            string                 `json:"node_id"`
	DisplayName       string                 `json:"display_name"`
	PublicKey         string                 `json:"public_key"`
	PrivateKey        string                 `json:"private_key"`
	AllowedBaseDirs   []string               `json:"allowed_base_dirs,omitempty"`
	ReadOnly          bool                   `json:"read_only,omitempty"`
	Paused            bool                   `json:"paused"`
	UploadCapMbps     int                    `json:"upload_cap_mbps"`
	AllowGroupCompute bool                   `json:"allow_group_compute"`
	Mounts            []protocol.FolderMount `json:"mounts,omitempty"`
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

func defaultHubsDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".config", "ourchive", "hubs")
	}
	return filepath.Join(home, ".config", "ourchive", "hubs")
}

func hubSlug(hubURL string) string {
	u, err := url.Parse(strings.TrimSpace(hubURL))
	raw := hubURL
	if err == nil && u.Host != "" {
		raw = u.Host
	}
	var b strings.Builder
	for _, r := range strings.ToLower(raw) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-.")
	if s == "" {
		return "default"
	}
	return s
}

func defaultHubConfigPath(hubURL string) string {
	return filepath.Join(defaultHubsDir(), hubSlug(hubURL)+".json")
}

func discoverHubConfigs() []string {
	dir := defaultHubsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	paths := []string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			continue
		}
		paths = append(paths, filepath.Join(dir, e.Name()))
	}
	return paths
}

func runListHubs() {
	paths := discoverHubConfigs()
	if len(paths) == 0 {
		fmt.Printf("No enrolled hubs found in %s\n", defaultHubsDir())
		return
	}
	fmt.Printf("Enrolled hubs (%s):\n", defaultHubsDir())
	for _, p := range paths {
		cfg, err := loadConfig(p)
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
		Compute:     probeCompute(),
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
	targetCfgPath := strings.TrimSpace(*cfgPath)
	if targetCfgPath == "" {
		targetCfgPath = defaultHubConfigPath(*hubURL)
	}

	hostname, _ := os.Hostname()
	displayName := strings.TrimSpace(*name)
	if displayName == "" {
		displayName = hostname
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		log.Fatalf("generate Ed25519 keypair: %v", err)
	}

	reqBody := protocol.EnrollRequest{
		Token:       *token,
		DisplayName: displayName,
		PublicKey:   base64.StdEncoding.EncodeToString(pub),
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		Compute:     probeCompute(),
	}

	payload, _ := json.Marshal(reqBody)
	endpoint := strings.TrimRight(*hubURL, "/") + "/api/agent/enroll"
	resp, err := http.Post(endpoint, "application/json", bytes.NewReader(payload))
	if err != nil {
		log.Fatalf("enroll request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		log.Fatalf("enrollment rejected (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var enrollRes protocol.EnrollResponse
	if err := json.NewDecoder(resp.Body).Decode(&enrollRes); err != nil {
		log.Fatalf("decode enroll response: %v", err)
	}

	cleanedAllow := []string{}
	for _, d := range allowDirs {
		if abs, err := filepath.Abs(d); err == nil {
			cleanedAllow = append(cleanedAllow, filepath.Clean(abs))
		}
	}

	cfg := AgentConfig{
		HubURL:          strings.TrimRight(*hubURL, "/"),
		NodeID:          enrollRes.NodeID,
		DisplayName:     enrollRes.DisplayName,
		PublicKey:       base64.StdEncoding.EncodeToString(pub),
		PrivateKey:      base64.StdEncoding.EncodeToString(priv),
		AllowedBaseDirs: cleanedAllow,
		ReadOnly:        *readOnly,
	}

	if err := saveConfig(targetCfgPath, &cfg); err != nil {
		log.Fatalf("save agent config: %v", err)
	}

	fmt.Printf("Enrolled machine %q (node_id: %s)\n", cfg.DisplayName, cfg.NodeID)
	fmt.Printf("Config saved to %s\n", targetCfgPath)
	fmt.Printf("Start the headless agent (connects to all enrolled hubs) with:\n  go run ./cmd/agent run\n")
}

func runDaemon(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("config", "", "Optional single config file (default: connect to all hubs in $HOME/.config/ourchive/hubs/)")
	forceReadOnly := fs.Bool("read-only", false, "Force read-only mode across all hubs (blocks all remote writes/deletes)")
	_ = fs.Parse(args)

	var configPaths []string
	if strings.TrimSpace(*cfgPath) != "" {
		configPaths = []string{strings.TrimSpace(*cfgPath)}
	} else {
		configPaths = discoverHubConfigs()
		if len(configPaths) == 0 {
			// Fallback to legacy ./agent.json if present.
			if _, err := os.Stat("agent.json"); err == nil {
				configPaths = []string{"agent.json"}
			}
		}
	}
	if len(configPaths) == 0 {
		log.Fatalf("no enrolled hubs found in %s (run `ourchive-agent enroll --hub <url> --token <tok>` first)", defaultHubsDir())
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if exePath, err := os.Executable(); err == nil {
		updater.CleanupOldBinary(exePath)
		gs := loadGlobalSettings()
		if gs.AutoUpdate && gs.UpdateURL != "" {
			go runAutoUpdateLoop(ctx, exePath, gs, stop)
		}
	}

	computeCaps := probeCompute()
	var wg sync.WaitGroup
	for _, path := range configPaths {
		cfg, err := loadConfig(path)
		if err != nil {
			log.Printf("skipping invalid config %s: %v", path, err)
			continue
		}
		privBytes, err := base64.StdEncoding.DecodeString(cfg.PrivateKey)
		if err != nil || len(privBytes) != ed25519.PrivateKeySize {
			log.Printf("skipping config %s: invalid private_key", path)
			continue
		}
		privKey := ed25519.PrivateKey(privBytes)
		if *forceReadOnly {
			cfg.ReadOnly = true
		}
		wg.Add(1)
		go func(p string, c *AgentConfig, pk ed25519.PrivateKey) {
			defer wg.Done()
			runHubLoop(ctx, p, c, pk, computeCaps)
		}(path, cfg, privKey)
	}

	wg.Wait()
	log.Println("ourchive agent stopped")
}

func runHubLoop(
	ctx context.Context,
	cfgPath string,
	cfg *AgentConfig,
	privKey ed25519.PrivateKey,
	computeCaps protocol.ComputeCapabilities,
) {
	var cfgMu sync.RWMutex
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		err := connectAndServe(ctx, cfgPath, cfg, &cfgMu, privKey, computeCaps)
		if ctx.Err() != nil {
			return
		}
		log.Printf("[%s] tunnel disconnected (%v); reconnecting in %s...", cfg.HubURL, err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 15*time.Second {
			backoff *= 2
			if backoff > 15*time.Second {
				backoff = 15 * time.Second
			}
		}
	}
}

func connectAndServe(
	ctx context.Context,
	cfgPath string,
	cfg *AgentConfig,
	cfgMu *sync.RWMutex,
	privKey ed25519.PrivateKey,
	computeCaps protocol.ComputeCapabilities,
) error {
	wsURL, err := buildTunnelURL(cfg.HubURL, cfg.NodeID, privKey)
	if err != nil {
		return err
	}

	dialCtx, cancelDial := context.WithTimeout(ctx, 15*time.Second)
	ws, _, err := websocket.Dial(dialCtx, wsURL, nil)
	cancelDial()
	if err != nil {
		return fmt.Errorf("dial hub: %w", err)
	}

	connCtx, cancelConn := context.WithCancel(ctx)
	defer cancelConn()

	netConn := websocket.NetConn(connCtx, ws, websocket.MessageBinary)
	ycfg := yamux.DefaultConfig()
	ycfg.LogOutput = io.Discard
	session, err := yamux.Client(netConn, ycfg)
	if err != nil {
		_ = ws.Close(websocket.StatusInternalError, "yamux client error")
		return fmt.Errorf("yamux client: %w", err)
	}
	defer session.Close()

	// Open Stream 1: persistent JSON-line control channel.
	ctrlStream, err := session.Open()
	if err != nil {
		return fmt.Errorf("open control stream: %w", err)
	}
	defer ctrlStream.Close()

	// Serve incoming Hub VFS streams (list, Range file streaming, uploads, mkdir, delete) over yamux.
	meter := vfs.NewTrafficMeter()
	vfsServer := vfs.NewAgentServer(func() vfs.ConfigSnapshot {
		cfgMu.RLock()
		defer cfgMu.RUnlock()
		mountsCopy := make([]protocol.FolderMount, len(cfg.Mounts))
		copy(mountsCopy, cfg.Mounts)
		return vfs.ConfigSnapshot{
			Paused:        cfg.Paused,
			ReadOnly:      cfg.ReadOnly,
			UploadCapMbps: cfg.UploadCapMbps,
			Mounts:        mountsCopy,
		}
	}, meter)
	vfsServer.Handle("POST /compute/convert", compute.NewConvertRunner(computeCaps))
	httpSrv := &http.Server{Handler: vfsServer}
	go func() {
		if err := httpSrv.Serve(session); err != nil && !strings.Contains(err.Error(), "closed") {
			log.Printf("agent vfs server stopped: %v", err)
		}
	}()
	defer httpSrv.Close()

	log.Printf("connected to hub %s as %q (%s)", cfg.HubURL, cfg.DisplayName, cfg.NodeID)

	var sendMu sync.Mutex
	enc := json.NewEncoder(ctrlStream)
	sendManifest := func(msgType protocol.ControlMsgType) error {
		upBps, downBps := meter.Sample()
		cfgMu.RLock()
		manifest := buildManifest(cfg, computeCaps, upBps, downBps)
		cfgMu.RUnlock()

		sendMu.Lock()
		defer sendMu.Unlock()
		return enc.Encode(protocol.ControlEnvelope{
			Type:     msgType,
			Manifest: &manifest,
		})
	}

	if err := sendManifest(protocol.ControlHello); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}

	// Periodic heartbeat goroutine.
	hbDone := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-connCtx.Done():
				hbDone <- nil
				return
			case <-ticker.C:
				if err := sendManifest(protocol.ControlHeartbeat); err != nil {
					hbDone <- err
					return
				}
			}
		}
	}()

	// Listen for Hub -> Agent config pushes.
	scanner := bufio.NewScanner(ctrlStream)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var env protocol.ControlEnvelope
		if err := json.Unmarshal(scanner.Bytes(), &env); err != nil {
			continue
		}
		if env.Type == protocol.ControlConfigPush && env.Config != nil {
			cfgMu.Lock()
			cfg.Paused = env.Config.Paused
			cfg.UploadCapMbps = env.Config.UploadCapMbps
			cfg.AllowGroupCompute = env.Config.AllowGroupCompute
			cfg.Mounts = validateMounts(env.Config.Mounts, cfg.AllowedBaseDirs)
			_ = saveConfig(cfgPath, cfg)
			cfgMu.Unlock()

			log.Printf("applied config from hub (paused=%v, upload_cap=%dMbps, gpu=%v, mounts=%d)",
				env.Config.Paused, env.Config.UploadCapMbps, env.Config.AllowGroupCompute, len(env.Config.Mounts))
			_ = sendManifest(protocol.ControlHeartbeat)
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}
	return io.EOF
}

func buildManifest(cfg *AgentConfig, compute protocol.ComputeCapabilities, upBps, downBps int64) protocol.NodeManifest {
	status := protocol.NodeStatusOnline
	if cfg.Paused {
		status = protocol.NodeStatusPaused
	}
	comp := compute
	if !cfg.AllowGroupCompute {
		comp.Enabled = compute.FFmpegPath != ""
	}
	return protocol.NodeManifest{
		NodeID:      cfg.NodeID,
		DisplayName: cfg.DisplayName,
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		Status:      status,
		Mounts:      validateMounts(cfg.Mounts, cfg.AllowedBaseDirs),
		Compute:     comp,
		Traffic: protocol.TrafficStats{
			UploadBps:     upBps,
			DownloadBps:   downBps,
			UploadCapMbps: cfg.UploadCapMbps,
		},
		UpdatedAt: time.Now().UTC(),
	}
}

// validateMounts enforces the local allowed_base_dirs defense-in-depth policy and verifies folder existence.
func validateMounts(mounts []protocol.FolderMount, allowedBaseDirs []string) []protocol.FolderMount {
	out := make([]protocol.FolderMount, len(mounts))
	for i, m := range mounts {
		m.Rejected = false
		m.RejectMsg = ""

		absPath, err := vfs.ExpandLocalPath(m.LocalPath)
		if err != nil {
			m.Rejected = true
			m.RejectMsg = "invalid path"
			out[i] = m
			continue
		}

		if len(allowedBaseDirs) > 0 && !isWithinAllowedDirs(absPath, allowedBaseDirs) {
			m.Rejected = true
			m.RejectMsg = "outside local allowed_base_dirs policy"
			out[i] = m
			continue
		}

		info, err := os.Stat(absPath)
		if err != nil {
			m.Rejected = true
			m.RejectMsg = "path not found on this machine (" + absPath + ")"
			out[i] = m
			continue
		}
		if m.Kind == "file" {
			if info.IsDir() {
				m.Rejected = true
				m.RejectMsg = "expected a file, got a directory"
				out[i] = m
				continue
			}
		} else {
			if !info.IsDir() {
				// Auto-treat explicit file paths as single-file references.
				m.Kind = "file"
			} else {
				m.Kind = "folder"
			}
		}

		out[i] = m
	}
	return out
}

func isWithinAllowedDirs(target string, allowed []string) bool {
	targetNorm := strings.ToLower(filepath.Clean(target))
	for _, base := range allowed {
		baseNorm := strings.ToLower(filepath.Clean(base))
		if targetNorm == baseNorm {
			return true
		}
		if strings.HasPrefix(targetNorm, baseNorm+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

func buildTunnelURL(hubURL, nodeID string, priv ed25519.PrivateKey) (string, error) {
	u, err := url.Parse(hubURL)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	default:
		u.Scheme = "ws"
	}
	u.Path = "/api/agent/tunnel"
	ts := time.Now().Unix()
	q := u.Query()
	q.Set("node_id", nodeID)
	q.Set("ts", fmt.Sprintf("%d", ts))
	q.Set("sig", tunnel.SignHandshake(nodeID, ts, priv))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func saveConfig(path string, cfg *AgentConfig) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func loadConfig(path string) (*AgentConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg AgentConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func probeCompute() protocol.ComputeCapabilities {
	return compute.ProbeCompute()
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
	fmt.Println("Usage: ourchive-agent <version|probe|list|enroll|run|update|service>")
}
