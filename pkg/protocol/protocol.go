package protocol

import "time"

// NodeStatus represents the live connectivity state of a contributor machine.
type NodeStatus string

const (
	NodeStatusOnline  NodeStatus = "online"
	NodeStatusOffline NodeStatus = "offline"
	NodeStatusPaused  NodeStatus = "paused"
)

// ComputeCapabilities advertises local encoding/transcoding hardware on an Agent.
type ComputeCapabilities struct {
	Enabled       bool     `json:"enabled"`
	FFmpegPath    string   `json:"ffmpeg_path,omitempty"`
	CPUModel      string   `json:"cpu_model,omitempty"`
	GPUModel      string   `json:"gpu_model,omitempty"`
	GPUModels     []string `json:"gpu_models,omitempty"`
	HWEncoders    []string `json:"hw_encoders,omitempty"` // e.g. h264_nvenc, hevc_nvenc, av1_nvenc
	CPUCores      int      `json:"cpu_cores"`
	MaxConcurrent int      `json:"max_concurrent"`
	ActiveJobs    int      `json:"active_jobs"`
}

// TrafficStats reports human-friendly upload/download throughput for a node.
type TrafficStats struct {
	UploadBps     int64 `json:"upload_bps"`
	DownloadBps   int64 `json:"download_bps"`
	UploadCapMbps int   `json:"upload_cap_mbps"`
}

// FolderMount describes a single local folder or individual file exposed by an Agent.
type FolderMount struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	LocalPath  string `json:"local_path"`
	Kind       string `json:"kind,omitempty"`       // "folder" (default) | "file"
	Visibility string `json:"visibility,omitempty"` // "inherit" (default) | "open" | "private"
	Access     string `json:"access,omitempty"`     // effective access for viewer: "all" | "view" | "download"
	Enabled    bool   `json:"enabled"`
	AllowSync  bool   `json:"allow_sync"`
	TotalBytes int64  `json:"total_bytes,omitempty"`
	Rejected   bool   `json:"rejected,omitempty"`
	RejectMsg  string `json:"reject_msg,omitempty"`
}

// NodeManifest is sent by the Agent on registration and periodic heartbeats.
type NodeManifest struct {
	NodeID      string              `json:"node_id"`
	DisplayName string              `json:"display_name"`
	OS          string              `json:"os"`
	Arch        string              `json:"arch"`
	Status      NodeStatus          `json:"status"`
	Mounts      []FolderMount       `json:"mounts"`
	Compute     ComputeCapabilities `json:"compute"`
	Traffic     TrafficStats        `json:"traffic"`
	UpdatedAt   time.Time           `json:"updated_at"`
}

// NodeConfigUpdate is pushed from the Hub to a connected Agent whenever the
// contributor edits their settings in the Web UI's Contribution settings pane.
type NodeConfigUpdate struct {
	Paused            bool          `json:"paused"`
	UploadCapMbps     int           `json:"upload_cap_mbps"`
	AllowGroupCompute bool          `json:"allow_group_compute"`
	Mounts            []FolderMount `json:"mounts"`
}

// ControlMsgType identifies messages exchanged on yamux stream 1 (control channel).
type ControlMsgType string

const (
	ControlHello      ControlMsgType = "hello"
	ControlHeartbeat  ControlMsgType = "heartbeat"
	ControlConfigPush ControlMsgType = "config_push"
	ControlConfigAck  ControlMsgType = "config_ack"
)

// ControlEnvelope is the JSON-line framed message sent over the control stream.
type ControlEnvelope struct {
	Type     ControlMsgType    `json:"type"`
	Manifest *NodeManifest     `json:"manifest,omitempty"`
	Config   *NodeConfigUpdate `json:"config,omitempty"`
	Error    string            `json:"error,omitempty"`
}

// EnrollRequest is sent by `ourchive-agent enroll` to bind an Ed25519 keypair to a node.
type EnrollRequest struct {
	Token       string              `json:"token"`
	DisplayName string              `json:"display_name"`
	PublicKey   string              `json:"public_key"` // base64-encoded Ed25519 public key
	OS          string              `json:"os"`
	Arch        string              `json:"arch"`
	Compute     ComputeCapabilities `json:"compute"`
}

// EnrollResponse is returned by the Hub after successful enrollment.
type EnrollResponse struct {
	NodeID      string `json:"node_id"`
	DisplayName string `json:"display_name"`
}

// FileEntry represents a file or directory inside a shared mount.
type FileEntry struct {
	Name     string   `json:"name"`
	Path     string   `json:"path"` // forward-slash relative path inside the mount
	IsDir    bool     `json:"is_dir"`
	Size     int64    `json:"size"`
	ModTime  string   `json:"mod_time"`
	MimeType string   `json:"mime_type"`
	Tags     []string `json:"tags,omitempty"`
}

// ListResponse is returned by Agent and Hub VFS directory listings.
type ListResponse struct {
	Path    string      `json:"path"`
	Entries []FileEntry `json:"entries"`
}
