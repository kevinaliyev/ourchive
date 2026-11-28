//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// showFatal pops a native error box. The Windows build has no console, so without this a startup
// failure would just look like the app closing by itself.
func showFatal(msg string) {
	title, _ := syscall.UTF16PtrFromString("Ourchive")
	text, _ := syscall.UTF16PtrFromString(msg)
	const mbIconError = 0x10
	_, _, _ = syscall.NewLazyDLL("user32.dll").NewProc("MessageBoxW").Call(
		0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(title)), mbIconError)
}
