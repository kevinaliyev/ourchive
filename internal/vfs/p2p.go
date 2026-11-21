package vfs

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

const partFileSuffix = ".ourchive.part"

type p2pOfferRequest struct {
	SDP               string   `json:"sdp"`
	STUNServers       []string `json:"stun_servers"`
	CanWrite          bool     `json:"can_write"`
	AllMountsAllowed  bool     `json:"all_mounts_allowed,omitempty"`
	AllowedReadMounts []string `json:"allowed_read_mounts,omitempty"`
}

type p2pFileRequest struct {
	Action  string   `json:"action"` // "get" | "zip" | "put"
	MountID string   `json:"mount_id"`
	Path    string   `json:"path"`
	Paths   []string `json:"paths,omitempty"`    // optional multi-select list for "zip"
	Size    int64    `json:"size,omitempty"`     // expected bytes for "put"
	Offset  int64    `json:"offset,omitempty"`   // resume byte offset for "get"
	IfMatch string   `json:"if_match,omitempty"` // expected file ETag for safe "get" resume
	Resume  bool     `json:"resume,omitempty"`   // resume existing .ourchive.part for "put"
	Done    bool     `json:"done,omitempty"`     // finalization marker for "put"
	SHA256  string   `json:"sha256,omitempty"`   // optional integrity check on "put" done
}

const p2pChunkSize = 64 * 1024 // 64 KB WebRTC DataChannel frames

func fileETag(info os.FileInfo) string {
	return fmt.Sprintf("%d-%d", info.Size(), info.ModTime().UTC().UnixNano())
}

// handleP2POffer establishes a direct WebRTC DataChannel between a browser and this Agent
// so file downloads, streaming .zip archives, and crash-safe resumable uploads go PC <-> Browser
// with zero Hub egress.
func (s *AgentServer) handleP2POffer(w http.ResponseWriter, r *http.Request) {
	cfg := s.GetConfig()
	if cfg.Paused {
		writeVFSError(w, errPaused)
		return
	}

	var req p2pOfferRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SDP == "" {
		http.Error(w, `{"error":"valid SDP offer is required"}`, http.StatusBadRequest)
		return
	}
	canWrite := req.CanWrite && !cfg.ReadOnly
	allowedRead := make(map[string]bool, len(req.AllowedReadMounts))
	for _, mid := range req.AllowedReadMounts {
		allowedRead[mid] = true
	}
	canReadMount := func(mountID string) bool {
		if req.AllMountsAllowed {
			return true
		}
		return allowedRead[mountID]
	}

	iceServers := []webrtc.ICEServer{}
	if len(req.STUNServers) > 0 {
		iceServers = append(iceServers, webrtc.ICEServer{URLs: req.STUNServers})
	} else {
		iceServers = append(iceServers, webrtc.ICEServer{URLs: []string{"stun:stun.l.google.com:19302"}})
	}

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{ICEServers: iceServers})
	if err != nil {
		http.Error(w, `{"error":"failed to create WebRTC peer connection"}`, http.StatusInternalServerError)
		return
	}

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateFailed ||
			state == webrtc.PeerConnectionStateClosed ||
			state == webrtc.PeerConnectionStateDisconnected {
			_ = pc.Close()
		}
	})

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() != "ourchive-vfs" {
			return
		}

		// Per-channel state for incoming crash-safe "put" uploads (.ourchive.part -> atomic rename).
		var putMu sync.Mutex
		var putFile *os.File
		var putRoot *os.Root
		var putWritten int64
		var putRelPath string
		var putPartPath string

		closePut := func() {
			if putFile != nil {
				_ = putFile.Sync()
				_ = putFile.Close()
				putFile = nil
			}
			if putRoot != nil {
				_ = putRoot.Close()
				putRoot = nil
			}
		}

		dc.OnClose(func() {
			// Keep .ourchive.part on disk so interrupted uploads can resume later!
			putMu.Lock()
			closePut()
			putMu.Unlock()
		})

		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			if !msg.IsString {
				putMu.Lock()
				defer putMu.Unlock()
				if putFile == nil {
					return
				}
				n, err := putFile.Write(msg.Data)
				if n > 0 {
					putWritten += int64(n)
					if s.Meter != nil {
						s.Meter.AddDownload(int64(n))
					}
				}
				if err != nil {
					closePut()
					errJSON, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
					_ = dc.SendText(string(errJSON))
				}
				return
			}

			var fileReq p2pFileRequest
			if err := json.Unmarshal(msg.Data, &fileReq); err != nil {
				_ = dc.SendText(`{"ok":false,"error":"invalid p2p request"}`)
				return
			}

			// Upload finalization frame: {"done":true,"sha256":"..."}
			if fileReq.Done && fileReq.Action == "" {
				putMu.Lock()
				if putFile == nil || putRoot == nil {
					putMu.Unlock()
					_ = dc.SendText(`{"ok":false,"error":"no active upload"}`)
					return
				}
				syncErr := putFile.Sync()
				closeErr := putFile.Close()
				putFile = nil
				root := putRoot
				putRoot = nil
				written := putWritten
				rel := putRelPath
				partRel := putPartPath
				putMu.Unlock()
				defer root.Close()

				if syncErr != nil || closeErr != nil {
					err := syncErr
					if err == nil {
						err = closeErr
					}
					errJSON, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
					_ = dc.SendText(string(errJSON))
					return
				}

				// Optional SHA-256 verification before atomic commit.
				if expectedHash := strings.ToLower(strings.TrimSpace(fileReq.SHA256)); expectedHash != "" {
					pf, err := root.Open(filepath.FromSlash(partRel))
					if err != nil {
						errJSON, _ := json.Marshal(map[string]any{"ok": false, "error": "failed to verify staged upload"})
						_ = dc.SendText(string(errJSON))
						return
					}
					h := sha256.New()
					_, _ = io.Copy(h, pf)
					_ = pf.Close()
					actualHash := hex.EncodeToString(h.Sum(nil))
					if actualHash != expectedHash {
						// Corrupt staging file: remove it so the next attempt starts clean.
						_ = root.Remove(filepath.FromSlash(partRel))
						errJSON, _ := json.Marshal(map[string]any{
							"ok":    false,
							"error": "upload integrity check failed (SHA-256 mismatch); please retry",
						})
						_ = dc.SendText(string(errJSON))
						return
					}
				}

				if err := root.Rename(filepath.FromSlash(partRel), filepath.FromSlash(rel)); err != nil {
					errJSON, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
					_ = dc.SendText(string(errJSON))
					return
				}

				doneJSON, _ := json.Marshal(map[string]any{
					"ok":    true,
					"done":  true,
					"path":  rel,
					"bytes": written,
				})
				_ = dc.SendText(string(doneJSON))
				return
			}

			switch fileReq.Action {
			case "get":
				if !canReadMount(fileReq.MountID) {
					_ = dc.SendText(`{"ok":false,"error":"read access denied for this folder"}`)
					return
				}
				go s.streamFileOverDataChannel(dc, fileReq)
			case "zip":
				if !canReadMount(fileReq.MountID) {
					_ = dc.SendText(`{"ok":false,"error":"read access denied for this folder"}`)
					return
				}
				go s.streamZipOverDataChannel(dc, fileReq)
			case "put":
				if !canWrite {
					_ = dc.SendText(`{"ok":false,"error":"write access denied on this machine"}`)
					return
				}
				putMu.Lock()
				closePut()
				root, _, err := s.openMountRoot(fileReq.MountID)
				if err != nil {
					putMu.Unlock()
					errJSON, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
					_ = dc.SendText(string(errJSON))
					return
				}
				rel, err := CleanRelPath(fileReq.Path)
				if err != nil || rel == "" {
					_ = root.Close()
					putMu.Unlock()
					_ = dc.SendText(`{"ok":false,"error":"valid file path is required"}`)
					return
				}
				if dir := path.Dir(rel); dir != "." && dir != "" {
					if err := root.MkdirAll(filepath.FromSlash(dir), 0o755); err != nil {
						_ = root.Close()
						putMu.Unlock()
						errJSON, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
						_ = dc.SendText(string(errJSON))
						return
					}
				}

				partRel := rel + partFileSuffix
				partOS := filepath.FromSlash(partRel)
				var f *os.File
				var existingOffset int64
				resumed := false

				if fileReq.Resume {
					if info, statErr := root.Stat(partOS); statErr == nil && !info.IsDir() {
						if fileReq.Size <= 0 || info.Size() <= fileReq.Size {
							if opened, openErr := root.OpenFile(partOS, os.O_WRONLY|os.O_APPEND, 0o644); openErr == nil {
								f = opened
								existingOffset = info.Size()
								resumed = existingOffset > 0
							}
						}
					}
				}
				if f == nil {
					created, createErr := root.Create(partOS)
					if createErr != nil {
						_ = root.Close()
						putMu.Unlock()
						errJSON, _ := json.Marshal(map[string]any{"ok": false, "error": createErr.Error()})
						_ = dc.SendText(string(errJSON))
						return
					}
					f = created
					existingOffset = 0
					resumed = false
				}

				putRoot = root
				putFile = f
				putWritten = existingOffset
				putRelPath = rel
				putPartPath = partRel
				putMu.Unlock()

				readyJSON, _ := json.Marshal(map[string]any{
					"ok":      true,
					"ready":   true,
					"offset":  existingOffset,
					"resumed": resumed,
				})
				_ = dc.SendText(string(readyJSON))
			default:
				_ = dc.SendText(`{"ok":false,"error":"unsupported p2p action"}`)
			}
		})
	})

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  req.SDP,
	}); err != nil {
		_ = pc.Close()
		http.Error(w, `{"error":"invalid remote SDP"}`, http.StatusBadRequest)
		return
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		_ = pc.Close()
		http.Error(w, `{"error":"failed to create SDP answer"}`, http.StatusInternalServerError)
		return
	}

	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		_ = pc.Close()
		http.Error(w, `{"error":"failed to set local SDP"}`, http.StatusInternalServerError)
		return
	}

	select {
	case <-gatherComplete:
	case <-time.After(6 * time.Second):
	case <-r.Context().Done():
		_ = pc.Close()
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"sdp": pc.LocalDescription().SDP,
	})
}

// dcStreamWriter adapts a WebRTC DataChannel into an io.Writer with 64 KB chunking,
// SCTP backpressure, and optional upload bandwidth capping.
type dcStreamWriter struct {
	dc             *webrtc.DataChannel
	meter          *TrafficMeter
	capBytesPerSec int64
	startTime      time.Time
	sent           int64
}

func (w *dcStreamWriter) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		for w.dc.BufferedAmount() > 1<<20 {
			if w.dc.ReadyState() != webrtc.DataChannelStateOpen {
				return total, io.ErrClosedPipe
			}
			time.Sleep(10 * time.Millisecond)
		}
		chunkLen := len(p)
		if chunkLen > p2pChunkSize {
			chunkLen = p2pChunkSize
		}
		if err := w.dc.Send(p[:chunkLen]); err != nil {
			return total, err
		}
		p = p[chunkLen:]
		total += chunkLen
		w.sent += int64(chunkLen)
		if w.meter != nil {
			w.meter.AddUpload(int64(chunkLen))
		}
		if w.capBytesPerSec > 0 {
			expected := time.Duration(float64(w.sent) / float64(w.capBytesPerSec) * float64(time.Second))
			if wait := expected - time.Since(w.startTime); wait > 0 {
				time.Sleep(wait)
			}
		}
	}
	return total, nil
}

func (s *AgentServer) streamZipOverDataChannel(dc *webrtc.DataChannel, req p2pFileRequest) {
	root, cfg, err := s.openMountRoot(req.MountID)
	if err != nil {
		errJSON, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
		_ = dc.SendText(string(errJSON))
		return
	}
	defer root.Close()

	zipName := "archive.zip"
	if len(req.Paths) > 0 {
		zipName = "selection.zip"
	} else {
		rel, err := CleanRelPath(req.Path)
		if err != nil {
			errJSON, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
			_ = dc.SendText(string(errJSON))
			return
		}
		if rel != "" {
			zipName = path.Base(rel) + ".zip"
		} else {
			for _, m := range cfg.Mounts {
				if m.ID == req.MountID && m.Name != "" {
					zipName = m.Name + ".zip"
					break
				}
			}
		}
		req.Path = rel
	}

	meta, _ := json.Marshal(map[string]any{
		"ok":        true,
		"name":      zipName,
		"size":      0,
		"mime_type": "application/zip",
	})
	if err := dc.SendText(string(meta)); err != nil {
		return
	}

	dst := &dcStreamWriter{
		dc:             dc,
		meter:          s.Meter,
		capBytesPerSec: int64(cfg.UploadCapMbps) * 125_000,
		startTime:      time.Now(),
	}
	zw := zip.NewWriter(dst)
	rootFS := root.FS()

	addFile := func(fsPath, entryName string, fi fs.FileInfo) error {
		if strings.HasSuffix(entryName, partFileSuffix) {
			return nil
		}
		hdr, err := zip.FileInfoHeader(fi)
		if err != nil {
			return err
		}
		hdr.Name = entryName
		if isAlreadyCompressed(entryName) {
			hdr.Method = zip.Store
		} else {
			hdr.Method = zip.Deflate
		}
		ew, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		src, err := rootFS.Open(fsPath)
		if err != nil {
			return nil
		}
		_, copyErr := io.Copy(ew, src)
		_ = src.Close()
		return copyErr
	}

	if len(req.Paths) > 0 {
		for _, rawP := range req.Paths {
			rel, err := CleanRelPath(rawP)
			if err != nil || rel == "" {
				continue
			}
			info, err := root.Stat(filepath.FromSlash(rel))
			if err != nil {
				continue
			}
			if !info.IsDir() {
				if err := addFile(rel, path.Base(rel), info); err != nil {
					_ = zw.Close()
					return
				}
				continue
			}
			basePrefix := path.Base(rel)
			_ = fs.WalkDir(rootFS, rel, func(p string, d fs.DirEntry, walkErr error) error {
				if walkErr != nil || d.IsDir() {
					return nil
				}
				sub := strings.TrimPrefix(p, rel+"/")
				if sub == "" || sub == "." {
					return nil
				}
				fi, err := d.Info()
				if err != nil {
					return nil
				}
				return addFile(p, basePrefix+"/"+sub, fi)
			})
		}
	} else {
		walkRoot := "."
		if req.Path != "" {
			walkRoot = req.Path
		}
		_ = fs.WalkDir(rootFS, walkRoot, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil || d.IsDir() {
				return nil
			}
			entryName := p
			if walkRoot != "." {
				entryName = strings.TrimPrefix(p, walkRoot+"/")
			}
			if entryName == "" || entryName == "." {
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				return nil
			}
			return addFile(p, entryName, fi)
		})
	}

	if err := zw.Close(); err != nil {
		errJSON, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
		_ = dc.SendText(string(errJSON))
		return
	}
	_ = dc.SendText(`{"done":true}`)
}

func (s *AgentServer) streamFileOverDataChannel(dc *webrtc.DataChannel, req p2pFileRequest) {
	_, cfg, err := s.findMount(req.MountID)
	if err != nil {
		errJSON, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
		_ = dc.SendText(string(errJSON))
		return
	}

	f, info, cleanup, err := s.openMountFile(req.MountID, req.Path)
	if err != nil {
		errJSON, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
		_ = dc.SendText(string(errJSON))
		return
	}
	defer cleanup()
	defer f.Close()

	if info.IsDir() {
		_ = dc.SendText(`{"ok":false,"error":"cannot stream directory over p2p"}`)
		return
	}

	etag := fileETag(info)
	startOffset := int64(0)
	resumed := false
	if req.Offset > 0 && req.Offset <= info.Size() && (req.IfMatch == "" || req.IfMatch == etag) {
		if _, seekErr := f.Seek(req.Offset, io.SeekStart); seekErr == nil {
			startOffset = req.Offset
			resumed = true
		}
	}

	meta, _ := json.Marshal(map[string]any{
		"ok":        true,
		"name":      info.Name(),
		"size":      info.Size(),
		"offset":    startOffset,
		"etag":      etag,
		"resumed":   resumed,
		"mime_type": detectMimeByExt(info.Name()),
	})
	if err := dc.SendText(string(meta)); err != nil {
		return
	}

	if startOffset < info.Size() {
		dst := &dcStreamWriter{
			dc:             dc,
			meter:          s.Meter,
			capBytesPerSec: int64(cfg.UploadCapMbps) * 125_000,
			startTime:      time.Now(),
		}
		if _, err := io.CopyBuffer(dst, f, make([]byte, p2pChunkSize)); err != nil {
			errJSON, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
			_ = dc.SendText(string(errJSON))
			return
		}
	}

	_ = dc.SendText(`{"done":true}`)
}
