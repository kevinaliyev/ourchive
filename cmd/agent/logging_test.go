package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInitAgentLoggerAndReadRecentLogs(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "logs", "agent.log")

	gotPath, closeFn, err := initAgentLogger(logPath)
	if err != nil {
		t.Fatalf("initAgentLogger failed: %v", err)
	}
	if gotPath != logPath {
		t.Fatalf("got log path %q, want %q", gotPath, logPath)
	}

	log.Println("line one")
	log.Println("line two")
	log.Println("line three")
	closeFn()

	recent, err := readRecentLogs(logPath, 2)
	if err != nil {
		t.Fatalf("readRecentLogs failed: %v", err)
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

	if err := rotateLogIfNeeded(logPath, 100); err != nil {
		t.Fatalf("rotateLogIfNeeded failed: %v", err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("expected original log path to be moved, got err=%v", err)
	}
	if _, err := os.Stat(logPath + ".1"); err != nil {
		t.Fatalf("expected rotated backup log.1 to exist: %v", err)
	}
}

func TestRunHubLoopStopsAfterMaxRetriesAndResetsOnConnect(t *testing.T) {
	ctx := context.Background()
	attempts := 0
	err := runHubLoopWithConnect(ctx, "https://example.test", 3, time.Millisecond, 2*time.Millisecond, func(ctx context.Context, onConnected func()) error {
		attempts++
		// Succeed once on attempt 2, then fail 3 consecutive times (attempts 3, 4, 5).
		if attempts == 2 {
			onConnected()
			return errors.New("connection dropped")
		}
		return errors.New("dial failed")
	})
	if err == nil {
		t.Fatalf("expected error after exhausting retries")
	}
	// Attempt 1 (fail 1/3), Attempt 2 (connected -> reset, then drop = fail 1/3),
	// Attempt 3 (fail 2/3), Attempt 4 (fail 3/3 -> stop).
	if attempts != 4 {
		t.Fatalf("expected 4 total attempts (1 fail, 1 connect+drop, 2 more fails), got %d", attempts)
	}
}

func TestPromptAfterFailure(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "agent.log")
	if err := os.WriteFile(logPath, []byte("2026/11/23 14:00:00 ERROR: test failure\n"), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	var out bytes.Buffer
	retry := promptAfterFailure(logPath, strings.NewReader("l\nr\n"), &out)
	if !retry {
		t.Fatalf("expected retry=true after 'l' then 'r'")
	}
	if !strings.Contains(out.String(), "ERROR: test failure") {
		t.Fatalf("expected recent log contents in prompt output, got:\n%s", out.String())
	}

	out.Reset()
	retry = promptAfterFailure(logPath, strings.NewReader("q\n"), &out)
	if retry {
		t.Fatalf("expected retry=false after 'q'")
	}
	if !strings.Contains(out.String(), logPath) {
		t.Fatalf("expected log path in exit output, got:\n%s", out.String())
	}
}
