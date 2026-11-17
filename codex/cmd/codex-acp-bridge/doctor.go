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

	"codex-acp-bridge/internal/codexapp"
	"codex-acp-bridge/internal/protocol"
)

// runDoctor performs a read-only handshake with app-server. It never starts a
// thread, never sends a prompt and never touches the model.
func runDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	bin := fs.String("codex-bin", envOr("CODEX_BIN", "codex"), "Codex CLI executable")
	var extra stringList
	fs.Var(&extra, "codex-arg", "extra `codex app-server` argument (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	report := protocol.Message{
		"adapter":        protocol.AgentName,
		"adapterVersion": protocol.VersionString,
		"platform":       runtime.GOOS + "/" + runtime.GOARCH,
		"codexBin":       *bin,
		"checks":         []protocol.Message{},
		"ok":             false,
	}
	checks := make([]protocol.Message, 0, 4)
	addCheck := func(name string, ok bool, detail string) {
		checks = append(checks, protocol.Message{"name": name, "ok": ok, "detail": detail})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	versionOutput, versionErr := exec.CommandContext(ctx, *bin, "--version").CombinedOutput()
	cliVersion := strings.TrimSpace(string(versionOutput))
	if versionErr != nil {
		addCheck("codex --version", false, versionErr.Error())
	} else {
		addCheck("codex --version", true, cliVersion)
		report["codexCli"] = cliVersion
	}

	start := time.Now()
	client, err := codexapp.Launch(ctx, codexapp.Config{
		Bin:       *bin,
		ExtraArgs: extra,
		OptOut:    []string{"thread/readState/changed"},
	})
	if err != nil {
		addCheck("app-server handshake", false, err.Error())
		report["checks"] = checks
		report["error"] = err.Error()
		printDoctor(report)
		return 1
	}
	defer client.Close()
	addCheck("app-server handshake", true,
		fmt.Sprintf("initialize + initialized in %s", time.Since(start).Round(time.Millisecond)))
	report["appServer"] = client.InitializeResult()
	report["checks"] = checks
	report["ok"] = versionErr == nil
	if versionErr != nil {
		printDoctor(report)
		return 1
	}
	printDoctor(report)
	return 0
}

// Doctor output is plain JSON on stdout; only the `serve` subcommand reserves
// stdout for protocol frames.
func printDoctor(report protocol.Message) {
	body, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		codexapp.Diagnostic("cannot render doctor report: %v", err)
		return
	}
	fmt.Println(string(body))
}
