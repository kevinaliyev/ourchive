package tunnel

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/hashicorp/yamux"

	"github.com/ourchive/ourchive/internal/auth"
	"github.com/ourchive/ourchive/internal/db"
	"github.com/ourchive/ourchive/pkg/protocol"
)

// TunnelTransport abstracts the multiplexed session layer so QUIC can be plugged in later.
type TunnelTransport interface {
	OpenStream() (net.Conn, error)
	AcceptStream() (net.Conn, error)
	Close() error
}

type yamuxTransport struct {
	*yamux.Session
}

func (y *yamuxTransport) OpenStream() (net.Conn, error)   { return y.Session.Open() }
func (y *yamuxTransport) AcceptStream() (net.Conn, error) { return y.Session.Accept() }

// ConnectedNode holds the live tunnel session and latest telemetry for an online Agent.
type ConnectedNode struct {
	NodeID    string
	Transport TunnelTransport

	mu       sync.RWMutex
	manifest protocol.NodeManifest
	configCh chan protocol.NodeConfigUpdate
	cancel   context.CancelFunc
}

// NodeView is the API representation of a contributor node returned to the Web UI.
type NodeView struct {
	ID                string                       `json:"id"`
	OwnerID           string                       `json:"owner_id"`
	OwnerName         string                       `json:"owner_name"`
	IsOwner           bool                         `json:"is_owner"`
	DisplayName       string                       `json:"display_name"`
	OS                string                       `json:"os"`
	Arch              string                       `json:"arch"`
	Status            protocol.NodeStatus          `json:"status"`
	Paused            bool                         `json:"paused"`
	Visibility        string                       `json:"visibility"` // "open" | "private"
	UploadCapMbps     int                          `json:"upload_cap_mbps"`
	AllowGroupCompute bool                         `json:"allow_group_compute"`
	Compute           protocol.ComputeCapabilities `json:"compute"`
	Traffic           protocol.TrafficStats        `json:"traffic"`
	Mounts            []protocol.FolderMount       `json:"mounts"`
	LastSeenAt        string                       `json:"last_seen_at,omitempty"`
	CreatedAt         string                       `json:"created_at"`
}

// Broker manages live Agent tunnels, enrollment tokens, and remote config push.
type Broker struct {
	store *db.Store

	mu    sync.RWMutex
	nodes map[string]*ConnectedNode
}

func NewBroker(store *db.Store) *Broker {
	// Reset any stale 'online' statuses from an ungraceful previous Hub exit.
	_, _ = store.DB.Exec(`UPDATE nodes SET status = 'offline' WHERE status != 'offline'`)
	return &Broker{
		store: store,
		nodes: make(map[string]*ConnectedNode),
	}
}

// SignHandshake signs `<nodeID>:<ts>` with the Agent's Ed25519 private key.
func SignHandshake(nodeID string, ts int64, priv ed25519.PrivateKey) string {
	msg := fmt.Sprintf("%s:%d", nodeID, ts)
	sig := ed25519.Sign(priv, []byte(msg))
	return base64.RawURLEncoding.EncodeToString(sig)
}

// VerifyHandshake validates an Agent tunnel handshake signature.
func VerifyHandshake(nodeID string, ts int64, sigB64, pubB64 string) bool {
	pub, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	msg := fmt.Sprintf("%s:%d", nodeID, ts)
	return ed25519.Verify(ed25519.PublicKey(pub), []byte(msg), sig)
}

func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// CreateEnrollToken creates a single-use 15-minute enrollment token for a contributor.
func (b *Broker) CreateEnrollToken(ctx context.Context, ownerID, displayName string) (string, time.Time, error) {
	rawBytes := make([]byte, 24)
	if _, err := rand.Read(rawBytes); err != nil {
		return "", time.Time{}, err
	}
	raw := "oc_" + base64.RawURLEncoding.EncodeToString(rawBytes)
	now := time.Now().UTC()
	expires := now.Add(15 * time.Minute)

	_, err := b.store.DB.ExecContext(ctx,
		`INSERT INTO enrollment_tokens (token_hash, owner_id, display_name, expires_at, created_at) VALUES (?, ?, ?, ?, ?)`,
		hashToken(raw), ownerID, strings.TrimSpace(displayName), expires.Format(time.RFC3339), now.Format(time.RFC3339),
	)
	if err != nil {
		return "", time.Time{}, err
	}
	return raw, expires, nil
}

// EnrollNode validates a one-time enrollment token and registers the Agent's Ed25519 identity.
func (b *Broker) EnrollNode(ctx context.Context, req protocol.EnrollRequest) (*protocol.EnrollResponse, error) {
	if req.Token == "" || req.PublicKey == "" {
		return nil, errors.New("token and public_key are required")
	}
	pub, err := base64.StdEncoding.DecodeString(req.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("invalid Ed25519 public_key")
	}

	th := hashToken(req.Token)
	var ownerID, presetName, expiresRaw string
	var usedAt sql.NullString
	err = b.store.DB.QueryRowContext(ctx,
		`SELECT owner_id, display_name, expires_at, used_at FROM enrollment_tokens WHERE token_hash = ?`,
		th,
	).Scan(&ownerID, &presetName, &expiresRaw, &usedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("invalid enrollment token")
	}
	if err != nil {
		return nil, err
	}
	if usedAt.Valid {
		return nil, errors.New("enrollment token already used")
	}
	expires, err := time.Parse(time.RFC3339, expiresRaw)
	if err != nil || time.Now().UTC().After(expires) {
		return nil, errors.New("enrollment token expired")
	}

	displayName := strings.TrimSpace(req.DisplayName)
	if displayName == "" {
		displayName = presetName
	}
	if displayName == "" {
		displayName = "unnamed-node"
	}

	capsJSON, _ := json.Marshal(req.Compute)
	nowStr := time.Now().UTC().Format(time.RFC3339)
	nodeID := uuid.NewString()

	tx, err := b.store.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE enrollment_tokens SET used_at = ? WHERE token_hash = ?`, nowStr, th); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO nodes (id, owner_id, display_name, public_key, status, os, arch, paused, upload_cap_mbps, allow_group_compute, capabilities_json, last_seen_at, created_at)
		VALUES (?, ?, ?, ?, 'offline', ?, ?, 0, 0, 0, ?, ?, ?)
	`, nodeID, ownerID, displayName, req.PublicKey, req.OS, req.Arch, string(capsJSON), nowStr, nowStr); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return &protocol.EnrollResponse{
		NodeID:      nodeID,
		DisplayName: displayName,
	}, nil
}

// HandleAgentTunnel upgrades an authenticated Agent WebSocket into a yamux server session.
func (b *Broker) HandleAgentTunnel(w http.ResponseWriter, r *http.Request) {
	nodeID := r.URL.Query().Get("node_id")
	tsStr := r.URL.Query().Get("ts")
	sig := r.URL.Query().Get("sig")

	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil || nodeID == "" || sig == "" {
		http.Error(w, "missing handshake parameters", http.StatusBadRequest)
		return
	}
	delta := time.Now().Unix() - ts
	if delta < -90 || delta > 90 {
		http.Error(w, "handshake timestamp expired", http.StatusUnauthorized)
		return
	}

	var pubKey, displayName string
	err = b.store.DB.QueryRowContext(r.Context(), `SELECT public_key, display_name FROM nodes WHERE id = ?`, nodeID).Scan(&pubKey, &displayName)
	if err != nil {
		http.Error(w, "unknown node", http.StatusUnauthorized)
		return
	}
	if !VerifyHandshake(nodeID, ts, sig, pubKey) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // Auth is enforced via Ed25519 signature, not Origin header.
	})
	if err != nil {
		log.Printf("tunnel websocket accept error for %s: %v", nodeID, err)
		return
	}

	connCtx, cancel := context.WithCancel(context.Background())
	netConn := websocket.NetConn(connCtx, ws, websocket.MessageBinary)

	ycfg := yamux.DefaultConfig()
	ycfg.LogOutput = io.Discard
	session, err := yamux.Server(netConn, ycfg)
	if err != nil {
		cancel()
		_ = ws.Close(websocket.StatusInternalError, "yamux init failed")
		return
	}

	cn := &ConnectedNode{
		NodeID:    nodeID,
		Transport: &yamuxTransport{Session: session},
		configCh:  make(chan protocol.NodeConfigUpdate, 8),
		cancel:    cancel,
	}

	b.mu.Lock()
	if prev, ok := b.nodes[nodeID]; ok {
		prev.cancel()
		_ = prev.Transport.Close()
	}
	b.nodes[nodeID] = cn
	b.mu.Unlock()

	log.Printf("agent connected: %s (%s)", displayName, nodeID)

	defer func() {
		cancel()
		_ = session.Close()
		b.mu.Lock()
		if cur, ok := b.nodes[nodeID]; ok && cur == cn {
			delete(b.nodes, nodeID)
		}
		b.mu.Unlock()
		_, _ = b.store.DB.Exec(`UPDATE nodes SET status = 'offline', last_seen_at = ? WHERE id = ?`,
			time.Now().UTC().Format(time.RFC3339), nodeID)
		log.Printf("agent disconnected: %s (%s)", displayName, nodeID)
	}()

	// Accept Stream 1: persistent JSON-line control channel.
	ctrlStream, err := cn.Transport.AcceptStream()
	if err != nil {
		return
	}
	defer ctrlStream.Close()

	// Writer goroutine for Hub -> Agent config pushes.
	writeErr := make(chan error, 1)
	go func() {
		enc := json.NewEncoder(ctrlStream)
		for {
			select {
			case <-connCtx.Done():
				writeErr <- nil
				return
			case cfg := <-cn.configCh:
				if err := enc.Encode(protocol.ControlEnvelope{
					Type:   protocol.ControlConfigPush,
					Config: &cfg,
				}); err != nil {
					writeErr <- err
					return
				}
			}
		}
	}()

	// Immediately push authoritative DB config to the newly connected Agent.
	if cfg, err := b.buildNodeConfig(connCtx, nodeID); err == nil {
		select {
		case cn.configCh <- cfg:
		default:
		}
	}

	// Reader loop for Agent -> Hub hello/heartbeat messages.
	scanner := bufio.NewScanner(ctrlStream)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var env protocol.ControlEnvelope
		if err := json.Unmarshal(scanner.Bytes(), &env); err != nil {
			continue
		}
		if env.Manifest != nil {
			cn.mu.Lock()
			cn.manifest = *env.Manifest
			cn.mu.Unlock()

			capsJSON, _ := json.Marshal(env.Manifest.Compute)
			nowStr := time.Now().UTC().Format(time.RFC3339)
			status := string(env.Manifest.Status)
			if status == "" {
				status = string(protocol.NodeStatusOnline)
			}
			_, _ = b.store.DB.Exec(`
				UPDATE nodes SET status = ?, os = ?, arch = ?, capabilities_json = ?, last_seen_at = ?
				WHERE id = ?
			`, status, env.Manifest.OS, env.Manifest.Arch, string(capsJSON), nowStr, nodeID)
		}
	}
}

func (b *Broker) buildNodeConfig(ctx context.Context, nodeID string) (protocol.NodeConfigUpdate, error) {
	var pausedInt, uploadCap, allowGPU int
	err := b.store.DB.QueryRowContext(ctx,
		`SELECT paused, upload_cap_mbps, allow_group_compute FROM nodes WHERE id = ?`, nodeID,
	).Scan(&pausedInt, &uploadCap, &allowGPU)
	if err != nil {
		return protocol.NodeConfigUpdate{}, err
	}

	mounts, err := b.ListMounts(ctx, nodeID)
	if err != nil {
		return protocol.NodeConfigUpdate{}, err
	}
	return protocol.NodeConfigUpdate{
		Paused:            pausedInt == 1,
		UploadCapMbps:     uploadCap,
		AllowGroupCompute: allowGPU == 1,
		Mounts:            mounts,
	}, nil
}

// PushConfig sends the latest DB configuration to a connected Agent if online.
func (b *Broker) PushConfig(ctx context.Context, nodeID string) {
	b.mu.RLock()
	cn, ok := b.nodes[nodeID]
	b.mu.RUnlock()
	if !ok {
		return
	}
	cfg, err := b.buildNodeConfig(ctx, nodeID)
	if err != nil {
		return
	}
	select {
	case cn.configCh <- cfg:
	default:
	}
}

// DisconnectNode closes an active Agent tunnel (used when deleting/revoking a node).
func (b *Broker) DisconnectNode(nodeID string) {
	b.mu.RLock()
	cn, ok := b.nodes[nodeID]
	b.mu.RUnlock()
	if ok {
		cn.cancel()
		_ = cn.Transport.Close()
	}
}

// ErrNodeOffline is returned when attempting to open a stream to a disconnected node.
var ErrNodeOffline = errors.New("node is offline")

// HTTPClientForNode returns an http.Client that dials streams directly over the node's live yamux tunnel.
func (b *Broker) HTTPClientForNode(nodeID string) (*http.Client, error) {
	b.mu.RLock()
	cn, ok := b.nodes[nodeID]
	b.mu.RUnlock()
	if !ok {
		return nil, ErrNodeOffline
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return cn.Transport.OpenStream()
		},
		DisableCompression: true,
		MaxIdleConns:       8,
		IdleConnTimeout:    30 * time.Second,
	}
	return &http.Client{Transport: transport}, nil
}

func (b *Broker) ListMounts(ctx context.Context, nodeID string) ([]protocol.FolderMount, error) {
	rows, err := b.store.DB.QueryContext(ctx,
		`SELECT id, name, local_path, kind, visibility, enabled, allow_sync FROM mounts WHERE node_id = ? ORDER BY created_at ASC`, nodeID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	mounts := []protocol.FolderMount{}
	for rows.Next() {
		var m protocol.FolderMount
		var en, syncInt int
		if err := rows.Scan(&m.ID, &m.Name, &m.LocalPath, &m.Kind, &m.Visibility, &en, &syncInt); err != nil {
			return nil, err
		}
		if m.Kind == "" {
			m.Kind = "folder"
		}
		if m.Visibility == "" {
			m.Visibility = "inherit"
		}
		m.Enabled = en == 1
		m.AllowSync = syncInt == 1
		mounts = append(mounts, m)
	}
	return mounts, rows.Err()
}

// ListNodesForUser returns all nodes with merged live telemetry & mount info.
func (b *Broker) ListNodesForUser(ctx context.Context, currentUser *auth.User) ([]NodeView, error) {
	rows, err := b.store.DB.QueryContext(ctx, `
		SELECT n.id, n.owner_id, u.display_name, n.display_name, n.os, n.arch, n.status,
		       n.paused, n.visibility, n.upload_cap_mbps, n.allow_group_compute, n.capabilities_json, n.last_seen_at, n.created_at
		FROM nodes n
		JOIN users u ON u.id = n.owner_id
		ORDER BY n.created_at ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	list := []NodeView{}
	for rows.Next() {
		var nv NodeView
		var statusStr, capsJSON string
		var pausedInt, allowGPUInt int
		var lastSeen sql.NullString
		if err := rows.Scan(
			&nv.ID, &nv.OwnerID, &nv.OwnerName, &nv.DisplayName, &nv.OS, &nv.Arch, &statusStr,
			&pausedInt, &nv.Visibility, &nv.UploadCapMbps, &allowGPUInt, &capsJSON, &lastSeen, &nv.CreatedAt,
		); err != nil {
			return nil, err
		}
		if nv.Visibility == "" {
			nv.Visibility = "open"
		}
		nv.IsOwner = currentUser != nil && currentUser.ID == nv.OwnerID
		nv.Paused = pausedInt == 1
		nv.AllowGroupCompute = allowGPUInt == 1
		if lastSeen.Valid {
			nv.LastSeenAt = lastSeen.String
		}
		_ = json.Unmarshal([]byte(capsJSON), &nv.Compute)
		list = append(list, nv)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	for i := range list {
		nv := &list[i]
		mounts, _ := b.ListMounts(ctx, nv.ID)
		nv.Mounts = mounts

		b.mu.RLock()
		cn, online := b.nodes[nv.ID]
		b.mu.RUnlock()

		if online {
			cn.mu.RLock()
			live := cn.manifest
			cn.mu.RUnlock()
			if nv.Paused {
				nv.Status = protocol.NodeStatusPaused
			} else {
				nv.Status = protocol.NodeStatusOnline
			}
			nv.Traffic = live.Traffic
			nv.Traffic.UploadCapMbps = nv.UploadCapMbps
			if live.Compute.CPUCores > 0 {
				nv.Compute = live.Compute
			}
			// Merge live mount status (e.g. rejected by local allowed_base_dirs)
			liveByID := map[string]protocol.FolderMount{}
			for _, lm := range live.Mounts {
				liveByID[lm.ID] = lm
			}
			for j := range nv.Mounts {
				if lm, ok := liveByID[nv.Mounts[j].ID]; ok {
					nv.Mounts[j].Rejected = lm.Rejected
					nv.Mounts[j].RejectMsg = lm.RejectMsg
					nv.Mounts[j].TotalBytes = lm.TotalBytes
				}
			}
		} else {
			nv.Status = protocol.NodeStatusOffline
			nv.Traffic = protocol.TrafficStats{UploadCapMbps: nv.UploadCapMbps}
		}
	}
	return list, nil
}

// IsNodeOwner returns true ONLY if the user strictly owns the machine.
// Even a host_admin is NOT an owner of another user's machine and cannot write,
// delete, mount, or trigger local OS actions on someone else's host.
func (b *Broker) IsNodeOwner(ctx context.Context, nodeID string, u *auth.User) (bool, error) {
	if u == nil {
		return false, nil
	}
	var ownerID string
	err := b.store.DB.QueryRowContext(ctx, `SELECT owner_id FROM nodes WHERE id = ?`, nodeID).Scan(&ownerID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, auth.ErrNotFound
	}
	if err != nil {
		return false, err
	}
	return ownerID == u.ID, nil
}

// CanShutoffNode returns true if the user owns the node OR is a host_admin
// (who is allowed to pause/shut off or disconnect any node from the Hub).
func (b *Broker) CanShutoffNode(ctx context.Context, nodeID string, u *auth.User) (bool, error) {
	if u == nil {
		return false, nil
	}
	owner, err := b.IsNodeOwner(ctx, nodeID, u)
	if err != nil {
		return false, err
	}
	return owner || u.Role == auth.RoleHostAdmin, nil
}

// CanManageNode is retained as an alias for CanShutoffNode (owner or host_admin).
func (b *Broker) CanManageNode(ctx context.Context, nodeID string, u *auth.User) (bool, error) {
	return b.CanShutoffNode(ctx, nodeID, u)
}
