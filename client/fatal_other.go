//go:build !windows

package main

// showFatal is a no-op where the app is started from a terminal or launcher that already shows
// stderr; the failure is still written to the log file.
func showFatal(string) {}
