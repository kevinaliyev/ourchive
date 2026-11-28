package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/ourchive/ourchive/internal/agentlog"
)

func runLogs(args []string) {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	lines := fs.Int("n", 80, "Number of recent log lines to print")
	pathOnly := fs.Bool("path", false, "Print only the log file path")
	logFile := fs.String("log-file", "", "Optional custom log file path")
	_ = fs.Parse(args)

	p := strings.TrimSpace(*logFile)
	if p == "" {
		p = agentlog.DefaultPath("agent.log")
	}
	if *pathOnly {
		fmt.Println(p)
		return
	}

	fmt.Printf("Agent log file: %s\n", p)
	content, err := agentlog.ReadRecent(p, *lines)
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
			} else if content, err := agentlog.ReadRecent(logPath, 50); err != nil {
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
