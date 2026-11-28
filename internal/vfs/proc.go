package vfs

import (
	"context"
	"os/exec"

	"github.com/ourchive/ourchive/internal/procutil"
)

// command builds a child process that won't flash a console window when this code runs inside the
// Windows desktop app (which has no console of its own).
func command(ctx context.Context, name string, args ...string) *exec.Cmd {
	return procutil.Hide(exec.CommandContext(ctx, name, args...))
}
