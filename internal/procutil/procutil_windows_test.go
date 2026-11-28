//go:build windows

package procutil

import (
	"os/exec"
	"testing"
)

func TestHideSetsNoWindowFlags(t *testing.T) {
	cmd := Hide(exec.Command("cmd", "/c", "exit", "0"))
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow || cmd.SysProcAttr.CreationFlags&createNoWindow == 0 {
		t.Fatalf("expected HideWindow and CREATE_NO_WINDOW, got %+v", cmd.SysProcAttr)
	}
	if err := cmd.Run(); err != nil {
		t.Fatalf("hidden command should still run: %v", err)
	}
}
