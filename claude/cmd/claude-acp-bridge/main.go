// claude-acp-bridge exposes Claude Code as a standard ACP v1 agent.
//
// Claude Code has no native ACP entry point; it speaks a bidirectional
// stream-json protocol when run with -p --input-format/--output-format. This
// binary is the adapter: it launches one `claude` child per session and
// translates between ACP and stream-json over stdio.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"claude-acp-bridge/internal/bridge"
	"claude-acp-bridge/internal/claudecode"
	"claude-acp-bridge/internal/protocol"
)

const usage = `claude-acp-bridge — ACP v1 adapter for Claude Code

Usage:
  claude-acp-bridge serve [flags]   Run as an ACP stdio agent (default)
  claude-acp-bridge doctor [flags]  Read-only startup self-check (no prompts)
  claude-acp-bridge version         Print version
  claude-acp-bridge help            Show this help

serve flags:
  --claude-bin PATH         Claude CLI executable (env CLAUDE_BIN, default "claude")
  --claude-arg ARG          Extra arg for the CLI launch (repeatable)
  --model ID                Default model id (env CLAUDE_ACP_MODEL)
  --permission-mode MODE    manual | acceptEdits | auto | bypassPermissions | plan | dontAsk
                            (env CLAUDE_ACP_PERMISSION_MODE, default "manual")
  --cwd PATH                Default working directory (env CLAUDE_ACP_CWD)
  --turn-timeout DURATION   Per-prompt budget (env CLAUDE_ACP_TURN_TIMEOUT, default 30m)
  --permission-fallback X   decline | accept, used when the client cannot answer
                            session/request_permission (env CLAUDE_ACP_PERMISSION_FALLBACK)
  --wire-log DIR            Record raw frames as JSONL (env CLAUDE_ACP_WIRE_LOG)

Diagnostics go to stderr; during serve, stdout carries ONLY ACP frames.
`

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		os.Exit(runServe(nil))
	}
	switch args[0] {
	case "version", "--version", "-v":
		fmt.Printf("%s %s\n", protocol.AgentName, protocol.VersionString)
	case "help", "-h", "--help":
		fmt.Print(usage)
	case "doctor":
		os.Exit(runDoctor(args[1:]))
	case "serve":
		os.Exit(runServe(args[1:]))
	default:
		if strings.HasPrefix(args[0], "-") {
			os.Exit(runServe(args)) // allow flags without the explicit subcommand
		}
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", args[0], usage)
		os.Exit(2)
	}
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}

type serveOptions struct {
	claudeBin          string
	claudeArgs         stringList
	model              string
	permissionMode     string
	cwd                string
	turnTimeout        time.Duration
	permissionFallback string
	wireLogDir         string
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func parseServeOptions(args []string) (serveOptions, error) {
	defaultTimeout := 30 * time.Minute
	if raw := os.Getenv("CLAUDE_ACP_TURN_TIMEOUT"); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil {
			defaultTimeout = parsed
		} else {
			return serveOptions{}, fmt.Errorf("invalid CLAUDE_ACP_TURN_TIMEOUT %q: %w", raw, err)
		}
	}
	options := serveOptions{
		claudeBin:          envOr("CLAUDE_BIN", "claude"),
		model:              envOr("CLAUDE_ACP_MODEL", ""),
		permissionMode:     envOr("CLAUDE_ACP_PERMISSION_MODE", "manual"),
		cwd:                envOr("CLAUDE_ACP_CWD", ""),
		turnTimeout:        defaultTimeout,
		permissionFallback: envOr("CLAUDE_ACP_PERMISSION_FALLBACK", "decline"),
		wireLogDir:         envOr("CLAUDE_ACP_WIRE_LOG", ""),
	}
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&options.claudeBin, "claude-bin", options.claudeBin, "Claude CLI executable")
	fs.Var(&options.claudeArgs, "claude-arg", "extra `claude` argument (repeatable)")
	fs.StringVar(&options.model, "model", options.model, "model id")
	fs.StringVar(&options.permissionMode, "permission-mode", options.permissionMode, "permission mode")
	fs.StringVar(&options.cwd, "cwd", options.cwd, "default cwd")
	fs.DurationVar(&options.turnTimeout, "turn-timeout", options.turnTimeout, "per-prompt budget")
	fs.StringVar(&options.permissionFallback, "permission-fallback", options.permissionFallback, "decline|accept")
	fs.StringVar(&options.wireLogDir, "wire-log", options.wireLogDir, "wire log directory")
	if err := fs.Parse(args); err != nil {
		return serveOptions{}, err
	}
	switch options.permissionMode {
	case "manual", "acceptEdits", "auto", "bypassPermissions", "plan", "dontAsk":
	default:
		return serveOptions{}, fmt.Errorf("invalid --permission-mode %q", options.permissionMode)
	}
	switch options.permissionFallback {
	case "decline", "accept":
	default:
		return serveOptions{}, fmt.Errorf("invalid --permission-fallback %q", options.permissionFallback)
	}
	return options, nil
}

func runServe(args []string) int {
	options, err := parseServeOptions(args)
	if err != nil {
		claudecode.Diagnostic("%v", err)
		return 2
	}
	log, err := protocol.OpenWireLog(options.wireLogDir, "claude-acp")
	if err != nil {
		claudecode.Diagnostic("cannot open wire log: %v", err)
		return 1
	}
	defer log.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server := bridge.New(claudecode.Runtime{}, os.Stdin, os.Stdout, bridge.Config{
		Bin:                options.claudeBin,
		ExtraArgs:          options.claudeArgs,
		Model:              options.model,
		PermissionMode:     options.permissionMode,
		CWD:                options.cwd,
		TurnTimeout:        options.turnTimeout,
		PermissionFallback: options.permissionFallback,
		Log:                log,
	})
	go func() {
		<-ctx.Done()
		server.Shutdown()
	}()

	if err := server.Run(); err != nil {
		claudecode.Diagnostic("ACP server stopped: %v", err)
		return 1
	}
	return 0
}
