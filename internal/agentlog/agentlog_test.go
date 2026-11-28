package agentlog_test

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ourchive/ourchive/internal/agentlog"
)

func TestInitAgentLoggerAndReadRecentLogs(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "logs", "agent.log")

	gotPath, closeFn, err := agentlog.Init(agentlog.Options{Path: logPath})
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	if gotPath != logPath {
		t.Fatalf("got log path %q, want %q", gotPath, logPath)
	}

	log.Println("line one")
	log.Println("line two")
	log.Println("line three")
	closeFn()

	recent, err := agentlog.ReadRecent(logPath, 2)
	if err != nil {
		t.Fatalf("ReadRecent failed: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(recent), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 recent lines, got %d (%q)", len(lines), recent)
	}
	if !strings.Contains(lines[0], "line two") || !strings.Contains(lines[1], "line three") {
		t.Fatalf("unexpected recent log lines: %q", recent)
	}
}

func TestRotateLogIfNeeded(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "agent.log")
	if err := os.WriteFile(logPath, bytes.Repeat([]byte("a"), 200), 0o600); err != nil {
		t.Fatalf("write seed log: %v", err)
	}

	if err := agentlog.Rotate(logPath, 100); err != nil {
		t.Fatalf("Rotate failed: %v", err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("expected original log path to be moved, got err=%v", err)
	}
	if _, err := os.Stat(logPath + ".1"); err != nil {
		t.Fatalf("expected rotated backup log.1 to exist: %v", err)
	}
}

func TestInitWithoutStderrStillWritesFile(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "client.log")
	_, closeFn, err := agentlog.Init(agentlog.Options{Path: logPath, Stderr: false})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	log.Println("gui line")
	closeFn()
	data, err := os.ReadFile(logPath)
	if err != nil || !strings.Contains(string(data), "gui line") {
		t.Fatalf("expected log line in file, got %q (err=%v)", data, err)
	}
}
