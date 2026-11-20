package vfs

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var winEnvRe = regexp.MustCompile(`%([^%]+)%`)

// ExpandLocalPath normalizes user-entered local paths with generous leeway:
//   - Trims whitespace and surrounding quotes (e.g. from Windows "Copy as path")
//   - Expands leading ~, ~/, or ~\ to the user's home directory
//   - Expands Unix-style $VAR / ${VAR} (falling back to UserHomeDir for $HOME)
//   - Expands Windows-style %VAR% (e.g. %USERPROFILE%, %ONEDRIVE%)
func ExpandLocalPath(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	// Strip matching single or double quotes if pasted from "Copy as path".
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			s = strings.TrimSpace(s[1 : len(s)-1])
		}
	}
	if s == "" {
		return "", errors.New("path is empty")
	}

	home, _ := os.UserHomeDir()

	// Expand Windows %VAR% syntax.
	s = winEnvRe.ReplaceAllStringFunc(s, func(match string) string {
		name := match[1 : len(match)-1]
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		if strings.EqualFold(name, "USERPROFILE") || strings.EqualFold(name, "HOME") {
			return home
		}
		return match
	})

	// Expand Unix $VAR / ${VAR} syntax.
	s = os.Expand(s, func(name string) string {
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		if strings.EqualFold(name, "HOME") || strings.EqualFold(name, "USERPROFILE") {
			return home
		}
		return "$" + name
	})

	// Expand leading ~.
	if s == "~" {
		if home == "" {
			return "", errors.New("could not determine home directory")
		}
		s = home
	} else if strings.HasPrefix(s, "~/") || strings.HasPrefix(s, `~\`) {
		if home == "" {
			return "", errors.New("could not determine home directory")
		}
		s = filepath.Join(home, s[2:])
	}

	abs, err := filepath.Abs(filepath.FromSlash(s))
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}
