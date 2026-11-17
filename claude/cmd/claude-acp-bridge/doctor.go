package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"claude-acp-bridge/internal/claudecode"
	"claude-acp-bridge/internal/protocol"
)

// runDoctor performs a read-only self check: it verifies the CLI version and
// the exact argv used to enter stream-json mode. It never sends a prompt,
// never starts a conversation and never calls the model.
func runDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	bin := fs.String("claude-bin", envOr("CLAUDE_BIN", "claude"), "Claude CLI executable")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	report := protocol.Message{
		"adapter":        protocol.AgentName,
		"adapterVersion": protocol.VersionString,
		"platform":       runtime.GOOS + "/" + runtime.GOARCH,
		"claudeBin":      *bin,
		"checks":         []protocol.Message{},
		"ok":             false,
	}
	checks := make([]protocol.Message, 0, 3)
	addCheck := func(name string, ok bool, detail string) {
		checks = append(checks, protocol.Message{"name": name, "ok": ok, "detail": detail})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	versionOutput, versionErr := exec.CommandContext(ctx, *bin, "--version").CombinedOutput()
	cliVersion := strings.TrimSpace(string(versionOutput))
	if versionErr != nil {
		addCheck("claude --version", false, versionErr.Error())
	} else {
		addCheck("claude --version", true, cliVersion)
		report["claudeCli"] = cliVersion
	}

	argv := claudecode.BuildArgs(claudecode.Config{SessionID: "00000000-0000-4000-8000-000000000000"})
	streamFlagsOK := containsAll(argv,
		"--input-format", "stream-json", "--output-format", "stream-json",
		"--verbose", "--permission-prompt-tool", "stdio")
	addCheck("stream-json argv", streamFlagsOK, strings.Join(argv, " "))
	report["streamJsonArgv"] = argv

	report["checks"] = checks
	report["ok"] = versionErr == nil && streamFlagsOK
	printDoctor(report)
	if !report["ok"].(bool) {
		return 1
	}
	return 0
}

func containsAll(argv []string, values ...string) bool {
	set := map[string]bool{}
	for _, arg := range argv {
		set[arg] = true
	}
	for _, value := range values {
		if !set[value] {
			return false
		}
	}
	return true
}

// Doctor output is plain JSON on stdout; only the `serve` subcommand reserves
// stdout for protocol frames.
func printDoctor(report protocol.Message) {
	body, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		claudecode.Diagnostic("cannot render doctor report: %v", err)
		return
	}
	fmt.Println(string(body))
}
