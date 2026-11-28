// Package agentlog owns the per-user log file shared by the headless agent CLI and the
// desktop client: OS-specific location, size-based rotation and tail reading.
package agentlog

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// MaxFileBytes is the size at which a log file is rotated to <name>.1 on startup.
const MaxFileBytes int64 = 5 * 1024 * 1024 // 5 MiB

// Dir returns the standard per-user logs directory for the current OS.
func Dir() string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "windows":
		if localAppData := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); localAppData != "" {
			return filepath.Join(localAppData, "Ourchive", "logs")
		}
		if home != "" {
			return filepath.Join(home, ".config", "ourchive", "logs")
		}
		return filepath.Join("logs")
	case "darwin":
		if home != "" {
			return filepath.Join(home, "Library", "Logs", "Ourchive")
		}
		return filepath.Join(".config", "ourchive", "logs")
	default:
		if stateHome := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); stateHome != "" {
			return filepath.Join(stateHome, "ourchive", "logs")
		}
		if home != "" {
			return filepath.Join(home, ".local", "state", "ourchive", "logs")
		}
		return filepath.Join(".config", "ourchive", "logs")
	}
}

// DefaultPath returns the default log file path for the given file name (e.g. "agent.log").
func DefaultPath(name string) string {
	if strings.TrimSpace(name) == "" {
		name = "agent.log"
	}
	return filepath.Join(Dir(), name)
}

// Options controls Init.
type Options struct {
	// Path overrides the log file location. Empty means DefaultPath(Name).
	Path string
	// Name is the file name used when Path is empty. Defaults to "agent.log".
	Name string
	// Stderr also mirrors log output to stderr. GUI builds without a console should leave this off.
	Stderr bool
}

// Init points the standard logger at a persistent per-user log file, rotating it first if it
// exceeds MaxFileBytes. It returns the resolved path and a function that restores stderr logging
// and closes the file. On error the returned path is still the one that was attempted.
func Init(opts Options) (string, func(), error) {
	logPath := strings.TrimSpace(opts.Path)
	if logPath == "" {
		logPath = DefaultPath(opts.Name)
	}

	if dir := filepath.Dir(logPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return logPath, func() {}, err
		}
	}

	if err := Rotate(logPath, MaxFileBytes); err != nil {
		// Non-fatal: continue appending to the existing log file.
		fmt.Fprintf(os.Stderr, "warning: could not rotate log file %s: %v\n", logPath, err)
	}

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return logPath, func() {}, err
	}

	// The file comes first: io.MultiWriter stops at the first failing writer, and stderr is not
	// writable in Windows GUI-subsystem builds.
	var out io.Writer = f
	if opts.Stderr {
		out = io.MultiWriter(f, os.Stderr)
	}
	log.SetFlags(log.LstdFlags)
	log.SetOutput(out)

	closeFn := func() {
		log.SetOutput(os.Stderr)
		_ = f.Close()
	}
	return logPath, closeFn, nil
}

// Rotate renames logPath to logPath+".1" when it is at least maxBytes long.
func Rotate(logPath string, maxBytes int64) error {
	info, err := os.Stat(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Size() < maxBytes {
		return nil
	}
	backup := logPath + ".1"
	_ = os.Remove(backup)
	return os.Rename(logPath, backup)
}

// ReadRecent returns the last maxLines lines from logPath.
func ReadRecent(logPath string, maxLines int) (string, error) {
	if maxLines <= 0 {
		maxLines = 80
	}
	f, err := os.Open(logPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	ring := make([]string, 0, maxLines)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if len(ring) < maxLines {
			ring = append(ring, line)
		} else {
			copy(ring, ring[1:])
			ring[maxLines-1] = line
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return strings.Join(ring, "\n"), nil
}
