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
	"github.com/ourchive/ourchive/internal/tunnel"
	"github.com/ourchive/ourchive/internal/vfs"
	"github.com/ourchive/ourchive/pkg/protocol"
)

var knownHWEncoders = []string{
	"h264_nvenc", "hevc_nvenc", "av1_nvenc",
	"h264_amf", "hevc_amf", "av1_amf",
	"h264_qsv", "hevc_qsv", "av1_qsv",
	"h264_videotoolbox", "hevc_videotoolbox",
}

// AgentConfig is persisted locally on the contributor machine (default: agent.json).
type AgentConfig struct {
	HubURL            string                 `json:"hub_url"`
	NodeID            string                 `json:"node_id"`
	DisplayName       string                 `json:"display_name"`
	PublicKey         string                 `json:"public_key"`
	PrivateKey        string                 `json:"private_key"`
	AllowedBaseDirs   []string               `json:"allowed_base_dirs,omitempty"`
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
	case "probe":
		runProbe()
	case "enroll":
		runEnroll(os.Args[2:])
	case "run":
		runDaemon(os.Args[2:])
	default:
		printUsage()
		os.Exit(1)
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
	cfgPath := fs.String("config", "agent.json", "Path to save agent configuration")
	var allowDirs multiFlag
	fs.Var(&allowDirs, "allow-dir", "Optional allowed base directory whitelist (repeatable)")
	_ = fs.Parse(args)

	if *token == "" {
		log.Fatal("--token is required")
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
	}

	if err := saveConfig(*cfgPath, &cfg); err != nil {
		log.Fatalf("save agent config: %v", err)
	}

	fmt.Printf("Enrolled machine %q (node_id: %s)\n", cfg.DisplayName, cfg.NodeID)
	fmt.Printf("Config saved to %s\n", *cfgPath)
	fmt.Printf("Start the headless agent with:\n  go run ./cmd/agent run --config %s\n", *cfgPath)
}

func runDaemon(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("config", "agent.json", "Path to agent configuration")
	_ = fs.Parse(args)

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("load config %s: %v", *cfgPath, err)
	}

	privBytes, err := base64.StdEncoding.DecodeString(cfg.PrivateKey)
	if err != nil || len(privBytes) != ed25519.PrivateKeySize {
		log.Fatalf("invalid private_key in %s", *cfgPath)
	}
	privKey := ed25519.PrivateKey(privBytes)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var cfgMu sync.RWMutex
	computeCaps := probeCompute()

	backoff := time.Second
	for {
		if ctx.Err() != nil {
			log.Println("ourchive agent stopped")
			return
		}

		err := connectAndServe(ctx, *cfgPath, cfg, &cfgMu, privKey, computeCaps)
		if ctx.Err() != nil {
			log.Println("ourchive agent stopped")
			return
		}
		log.Printf("tunnel disconnected (%v); reconnecting in %s...", err, backoff)
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

func printUsage() {
	fmt.Println("Usage: ourchive-agent <probe|enroll|run>")
}
