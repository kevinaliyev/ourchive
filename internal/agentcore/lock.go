package agentcore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ErrAlreadyRunning means another agent process (the CLI or the desktop app) holds the lock.
var ErrAlreadyRunning = errors.New("another Ourchive agent is already running on this computer")

// InstanceLock guarantees a single contributor agent per user. Two agents would connect the same
// node to its hubs twice and fight over the tunnel.
type InstanceLock struct {
	f    *os.File
	path string
}

// LockPath is the lock file shared by every agent build.
func LockPath() string { return filepath.Join(ConfigDir(), "agent.lock") }

// AcquireInstanceLock takes the per-user agent lock without blocking. On failure because another
// process holds it, the error wraps ErrAlreadyRunning and names that process when known. The lock
// is released by Release or automatically when the process exits.
func AcquireInstanceLock(owner string) (*InstanceLock, error) {
	path := LockPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		holder := readHolder(path)
		f.Close()
		if holder != "" {
			return nil, fmt.Errorf("%w (%s)", ErrAlreadyRunning, holder)
		}
		return nil, ErrAlreadyRunning
	}
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(owner+" pid "+strconv.Itoa(os.Getpid())+"\n"), 0)
	return &InstanceLock{f: f, path: path}, nil
}

// AcquireInstanceLockWait retries AcquireInstanceLock until timeout. It covers a self-update
// restart, where the new process starts moments before the old one lets go.
func AcquireInstanceLockWait(owner string, timeout time.Duration) (*InstanceLock, error) {
	deadline := time.Now().Add(timeout)
	for {
		l, err := AcquireInstanceLock(owner)
		if err == nil || !errors.Is(err, ErrAlreadyRunning) || time.Now().After(deadline) {
			return l, err
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// Release drops the lock.
func (l *InstanceLock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = l.f.Truncate(0)
	_ = unlockFile(l.f)
	_ = l.f.Close()
	l.f = nil
}

func readHolder(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
