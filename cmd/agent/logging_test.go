package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
