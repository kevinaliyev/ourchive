package procutil

import (
	"os/exec"
	"testing"
)

func TestHideReturnsSameCommand(t *testing.T) {
	cmd := exec.Command("go", "version")
	if Hide(cmd) != cmd {
		t.Fatal("Hide must return its argument")
	}
}
