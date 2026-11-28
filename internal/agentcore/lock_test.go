package agentcore

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestInstanceLockIsExclusive(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))

	first, err := AcquireInstanceLock("desktop app")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	_, err = AcquireInstanceLock("ourchive-agent")
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second acquire should fail with ErrAlreadyRunning, got %v", err)
	}
	if !strings.Contains(err.Error(), "desktop app") {
		t.Fatalf("error should name the holder, got %q", err)
	}

	go func() {
		time.Sleep(100 * time.Millisecond)
		first.Release()
	}()
	second, err := AcquireInstanceLockWait("ourchive-agent", 5*time.Second)
	if err != nil {
		t.Fatalf("waiting acquire after release: %v", err)
	}
	second.Release()
	second.Release() // idempotent
}
