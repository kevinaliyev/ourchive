package agentcore

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"

	"github.com/ourchive/ourchive/internal/compute"
	"github.com/ourchive/ourchive/internal/tunnel"
	"github.com/ourchive/ourchive/internal/vfs"
	"github.com/ourchive/ourchive/pkg/protocol"
)

func connectAndServe(
	ctx context.Context,
	cfgPath string,
	cfg *AgentConfig,
	cfgMu *sync.RWMutex,
	privKey ed25519.PrivateKey,
	computeCaps protocol.ComputeCapabilities,
	onConnected func(),
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
	if onConnected != nil {
		onConnected()
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
			_ = SaveConfig(cfgPath, cfg)
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

		if len(allowedBaseDirs) > 0 {
			checkPath := absPath
			if evalPath, evalErr := filepath.EvalSymlinks(absPath); evalErr == nil {
				checkPath = evalPath
			}
			if !isWithinAllowedDirs(checkPath, allowedBaseDirs) {
				m.Rejected = true
				m.RejectMsg = "outside local allowed_base_dirs policy"
				out[i] = m
				continue
			}
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

