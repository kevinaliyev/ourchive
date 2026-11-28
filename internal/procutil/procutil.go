// Package procutil holds small helpers for starting child processes.
package procutil

import "os/exec"

// Hide stops cmd from opening a console window. It matters for the Windows desktop app, which has
// no console of its own: without it every child console program (ffmpeg, PowerShell, pandoc, ...)
// flashes a window on screen. On other platforms it does nothing. It returns cmd for chaining.
func Hide(cmd *exec.Cmd) *exec.Cmd {
	hide(cmd)
	return cmd
}
