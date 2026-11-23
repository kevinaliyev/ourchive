package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const maxLogFileBytes int64 = 5 * 1024 * 1024 // 5 MiB

// defaultLogDir returns the standard per-user logs directory for the current OS.
func defaultLogDir() string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "windows":
		if localAppData := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); localAppData != "" {
			return filepath.Join(localAppData, "Ourchive", "logs")
		}
		if home != "" {
			return filepath.Join(home, ".config", "ourchive", "logs")
		}
		return filepath.Join("logs")
	case "darwin":
		if home != "" {
			return filepath.Join(home, "Library", "Logs", "Ourchive")
		}
		return filepath.Join(".config", "ourchive", "logs")
	default:
		if stateHome := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); stateHome != "" {
			return filepath.Join(stateHome, "ourchive", "logs")
		}
		if home != "" {
			return filepath.Join(home, ".local", "state", "ourchive", "logs")
		}
		return filepath.Join(".config", "ourchive", "logs")
	}
}

func defaultLogPath() string {
	return filepath.Join(defaultLogDir(), "agent.log")
}

// initAgentLogger sets up standard log output to write to both stderr and a persistent
// user log file, rotating the log file if it exceeds maxLogFileBytes.
func initAgentLogger(customPath string) (string, func(), error) {
	logPath := strings.TrimSpace(customPath)
	if logPath == "" {
		logPath = defaultLogPath()
	}

	if dir := filepath.Dir(logPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return logPath, func() {}, err
		}
	}

	if err := rotateLogIfNeeded(logPath, maxLogFileBytes); err != nil {
		// Non-fatal: continue appending to the existing log file.
		fmt.Fprintf(os.Stderr, "warning: could not rotate log file %s: %v\n", logPath, err)
	}

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return logPath, func() {}, err
	}

	log.SetFlags(log.LstdFlags)
	log.SetOutput(io.MultiWriter(os.Stderr, f))

	closeFn := func() {
		log.SetOutput(os.Stderr)
		_ = f.Close()
	}
	return logPath, closeFn, nil
}

func rotateLogIfNeeded(logPath string, maxBytes int64) error {
	info, err := os.Stat(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Size() < maxBytes {
		return nil
	}
	backup := logPath + ".1"
	_ = os.Remove(backup)
	return os.Rename(logPath, backup)
}

// readRecentLogs returns the last maxLines lines from logPath.
func readRecentLogs(logPath string, maxLines int) (string, error) {
	if maxLines <= 0 {
		maxLines = 80
	}
	f, err := os.Open(logPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	ring := make([]string, 0, maxLines)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if len(ring) < maxLines {
			ring = append(ring, line)
		} else {
			copy(ring, ring[1:])
			ring[maxLines-1] = line
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return strings.Join(ring, "\n"), nil
}

func runLogs(args []string) {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	lines := fs.Int("n", 80, "Number of recent log lines to print")
	pathOnly := fs.Bool("path", false, "Print only the log file path")
	logFile := fs.String("log-file", "", "Optional custom log file path")
	_ = fs.Parse(args)

	p := strings.TrimSpace(*logFile)
	if p == "" {
		p = defaultLogPath()
	}
	if *pathOnly {
		fmt.Println(p)
		return
	}

	fmt.Printf("Agent log file: %s\n", p)
	content, err := readRecentLogs(p, *lines)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("(no logs recorded yet)")
			return
		}
		log.Fatalf("read logs: %v", err)
	}
	if strings.TrimSpace(content) == "" {
		fmt.Println("(log file is empty)")
		return
	}
	fmt.Println(content)
}

func isInteractiveTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}

// promptAfterFailure keeps an interactive shell open after all hubs exhaust retries,
// letting the user retry, print recent logs, or exit. Returns true if the user chose retry.
func promptAfterFailure(logPath string, in io.Reader, out io.Writer) bool {
	scanner := bufio.NewScanner(in)
	for {
		fmt.Fprintf(out, "\nAll enrolled hubs failed to connect.\n")
		if logPath != "" {
			fmt.Fprintf(out, "Log file: %s\n", logPath)
		}
		fmt.Fprintf(out, "Choose an action: [r] retry, [l] print recent logs, [q] exit: ")

		if !scanner.Scan() {
			fmt.Fprintln(out)
			return false
		}
		choice := strings.ToLower(strings.TrimSpace(scanner.Text()))
		switch choice {
		case "r", "retry":
			fmt.Fprintln(out, "Retrying hub connections...")
			return true
		case "l", "log", "logs":
			fmt.Fprintln(out, "\n--- Recent Agent Logs ---")
			if logPath == "" {
				fmt.Fprintln(out, "(log file not configured)")
			} else if content, err := readRecentLogs(logPath, 50); err != nil {
				fmt.Fprintf(out, "(unable to read %s: %v)\n", logPath, err)
			} else if strings.TrimSpace(content) == "" {
				fmt.Fprintln(out, "(log file is empty)")
			} else {
				fmt.Fprintln(out, content)
			}
			fmt.Fprintln(out, "-------------------------")
		case "q", "quit", "exit", "":
			if logPath != "" {
				fmt.Fprintf(out, "Exiting. Full logs saved at: %s\n", logPath)
			}
			return false
		default:
			fmt.Fprintf(out, "Unknown option %q. Enter r (retry), l (logs), or q (exit).\n", choice)
		}
	}
}
