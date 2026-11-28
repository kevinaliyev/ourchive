//go:build windows

package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"

	"github.com/ourchive/ourchive/internal/procutil"
)

const schemeKey = `HKCU\Software\Classes\ourchive`

// registerURLScheme tells Windows that this executable handles ourchive:// links, so the hub
// website's "Open Ourchive app" button works for a copy that was built or unzipped rather than run
// through the installer. It writes only under the current user's HKCU hive (no admin rights) and is
// skipped when the registration already points here. The installer registers the same scheme; the
// most recently started copy wins.
func registerURLScheme() {
	exe, err := os.Executable()
	if err != nil {
		log.Printf("url scheme: cannot find own path: %v", err)
		return
	}
	command := fmt.Sprintf(`"%s" "%%1"`, exe)

	out, _ := procutil.Hide(exec.Command("reg", "query", schemeKey+`\shell\open\command`, "/ve")).Output()
	if strings.Contains(string(out), command) {
		return
	}

	for _, args := range [][]string{
		{"add", schemeKey, "/ve", "/d", "URL:Ourchive", "/f"},
		{"add", schemeKey, "/v", "URL Protocol", "/d", "", "/f"},
		{"add", schemeKey + `\shell\open\command`, "/ve", "/d", command, "/f"},
	} {
		if out, err := procutil.Hide(exec.Command("reg", args...)).CombinedOutput(); err != nil {
			log.Printf("url scheme: reg %s failed: %v: %s", strings.Join(args[:2], " "), err, strings.TrimSpace(string(out)))
			return
		}
	}
	log.Printf("url scheme: ourchive:// now opens %s", exe)
}
