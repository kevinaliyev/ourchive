package vfs

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/pion/webrtc/v4"
)

type p2pOfferRequest struct {
	SDP         string   `json:"sdp"`
	STUNServers []string `json:"stun_servers"`
}

type p2pFileRequest struct {
	Action  string `json:"action"` // "get"
	MountID string `json:"mount_id"`
	Path    string `json:"path"`
}

const p2pChunkSize = 64 * 1024 // 64 KB WebRTC DataChannel frames

// handleP2POffer establishes a direct WebRTC DataChannel between a viewer's browser
// and this Agent so file bytes stream directly PC-to-Browser with zero Hub egress.
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
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			if !msg.IsString {
				return
			}
			var fileReq p2pFileRequest
			if err := json.Unmarshal(msg.Data, &fileReq); err != nil || fileReq.Action != "get" {
				_ = dc.SendText(`{"ok":false,"error":"invalid p2p file request"}`)
				return
			}
			go s.streamFileOverDataChannel(dc, fileReq)
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

	meta, _ := json.Marshal(map[string]any{
		"ok":        true,
		"name":      info.Name(),
		"size":      info.Size(),
		"mime_type": detectMimeByExt(info.Name()),
	})
	if err := dc.SendText(string(meta)); err != nil {
		return
	}

	buf := make([]byte, p2pChunkSize)
	capBytesPerSec := int64(cfg.UploadCapMbps) * 125_000
	startTime := time.Now()
	var sent int64

	for {
		// Backpressure: pause briefly if SCTP send buffer exceeds 1 MB.
		for dc.BufferedAmount() > 1<<20 {
			if dc.ReadyState() != webrtc.DataChannelStateOpen {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}

		n, readErr := f.Read(buf)
		if n > 0 {
			if err := dc.Send(buf[:n]); err != nil {
				return
			}
			sent += int64(n)
			if s.Meter != nil {
				s.Meter.AddUpload(int64(n))
			}
			if capBytesPerSec > 0 {
				expected := time.Duration(float64(sent) / float64(capBytesPerSec) * float64(time.Second))
				if wait := expected - time.Since(startTime); wait > 0 {
					time.Sleep(wait)
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			errJSON, _ := json.Marshal(map[string]any{"ok": false, "error": readErr.Error()})
			_ = dc.SendText(string(errJSON))
			return
		}
	}

	_ = dc.SendText(`{"done":true}`)
}
