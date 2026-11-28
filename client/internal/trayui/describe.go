package trayui

import (
	"fmt"
	"strings"

	"github.com/ourchive/ourchive/internal/agentstatus"
	"github.com/ourchive/ourchive/internal/clientcore"
)

// Describe turns client state into the tray's status row and tooltip.
func Describe(st clientcore.State) (label, tooltip string) {
	if !st.Platform.Contributor {
		return "Not contributing on this computer", "Ourchive"
	}
	total := len(st.Agent)
	if total == 0 {
		return "Not contributing yet", "Ourchive"
	}
	connected := 0
	var problems []string
	for _, h := range st.Agent {
		switch h.State {
		case agentstatus.StateConnected:
			connected++
		case agentstatus.StateRetrying:
			problems = append(problems, fmt.Sprintf("%s: reconnecting (%d/%d)", hostOf(h.HubURL), h.Failures, h.MaxRetries))
		case agentstatus.StateFailed:
			problems = append(problems, hostOf(h.HubURL)+": offline")
		case agentstatus.StateConnecting:
			problems = append(problems, hostOf(h.HubURL)+": connecting")
		}
	}
	label = fmt.Sprintf("Contributing to %d of %d hub", connected, total)
	if total != 1 {
		label += "s"
	}
	tooltip = "Ourchive: " + label
	if len(problems) > 0 {
		tooltip += "\n" + strings.Join(problems, "\n")
	}
	return label, tooltip
}

func hostOf(hubURL string) string {
	if i := strings.Index(hubURL, "://"); i >= 0 {
		return hubURL[i+3:]
	}
	return hubURL
}
