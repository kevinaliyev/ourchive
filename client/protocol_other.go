//go:build !windows

package main

// registerURLScheme is a no-op: macOS reads the scheme from the app bundle's Info.plist and Linux
// from the .desktop file, both written by the packaging step.
func registerURLScheme() {}
