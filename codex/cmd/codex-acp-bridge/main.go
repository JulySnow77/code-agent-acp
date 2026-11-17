// codex-acp-bridge exposes OpenAI Codex as a standard ACP v1 agent.
//
// Codex CLI has no native `codex acp` entry point; it speaks its own
// "app-server" JSON-RPC-lite protocol. This binary is the adapter: it launches
// `codex app-server` as a child process and translates between the two
// protocols over stdio.
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

	"codex-acp-bridge/internal/bridge"
	"codex-acp-bridge/internal/codexapp"
	"codex-acp-bridge/internal/protocol"
)

const usage = `codex-acp-bridge — ACP v1 adapter for the Codex CLI

Usage:
  codex-acp-bridge serve [flags]   Run as an ACP stdio agent (default)
  codex-acp-bridge doctor [flags]  Read-only startup self-check (no prompts)
  codex-acp-bridge version         Print version
  codex-acp-bridge help            Show this help

serve flags:
  --codex-bin PATH          Codex CLI executable (env CODEX_BIN, default "codex")
  --codex-arg ARG           Extra arg for ` + "`codex app-server`" + ` (repeatable)
  --approval-policy NAME    on-request | never | untrusted (env CODEX_ACP_APPROVAL_POLICY)
  --sandbox MODE            read-only | workspace-write | danger-full-access (env CODEX_ACP_SANDBOX)
  --model ID                Default model id (env CODEX_ACP_MODEL)
  --cwd PATH                Default working directory (env CODEX_ACP_CWD)
  --turn-timeout DURATION   Per-prompt budget (env CODEX_ACP_TURN_TIMEOUT, default 30m)
  --permission-fallback X   decline | accept, used when the client cannot answer
                            session/request_permission (env CODEX_ACP_PERMISSION_FALLBACK)
  --wire-log DIR            Record raw frames as JSONL (env CODEX_ACP_WIRE_LOG)

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
	codexBin           string
	codexArgs          stringList
	approvalPolicy     string
	sandbox            string
	model              string
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
	if raw := os.Getenv("CODEX_ACP_TURN_TIMEOUT"); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil {
			defaultTimeout = parsed
		} else {
			return serveOptions{}, fmt.Errorf("invalid CODEX_ACP_TURN_TIMEOUT %q: %w", raw, err)
		}
	}
	options := serveOptions{
		codexBin:           envOr("CODEX_BIN", "codex"),
		approvalPolicy:     envOr("CODEX_ACP_APPROVAL_POLICY", "on-request"),
		sandbox:            envOr("CODEX_ACP_SANDBOX", "workspace-write"),
		model:              envOr("CODEX_ACP_MODEL", ""),
		cwd:                envOr("CODEX_ACP_CWD", ""),
		turnTimeout:        defaultTimeout,
		permissionFallback: envOr("CODEX_ACP_PERMISSION_FALLBACK", "decline"),
		wireLogDir:         envOr("CODEX_ACP_WIRE_LOG", ""),
	}
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&options.codexBin, "codex-bin", options.codexBin, "Codex CLI executable")
	fs.Var(&options.codexArgs, "codex-arg", "extra `codex app-server` argument (repeatable)")
	fs.StringVar(&options.approvalPolicy, "approval-policy", options.approvalPolicy, "approval policy")
	fs.StringVar(&options.sandbox, "sandbox", options.sandbox, "sandbox mode")
	fs.StringVar(&options.model, "model", options.model, "model id")
	fs.StringVar(&options.cwd, "cwd", options.cwd, "default cwd")
	fs.DurationVar(&options.turnTimeout, "turn-timeout", options.turnTimeout, "per-prompt budget")
	fs.StringVar(&options.permissionFallback, "permission-fallback", options.permissionFallback, "decline|accept")
	fs.StringVar(&options.wireLogDir, "wire-log", options.wireLogDir, "wire log directory")
	if err := fs.Parse(args); err != nil {
		return serveOptions{}, err
	}
	switch options.approvalPolicy {
	case "on-request", "never", "untrusted":
	default:
		return serveOptions{}, fmt.Errorf("invalid --approval-policy %q", options.approvalPolicy)
	}
	switch options.sandbox {
	case "read-only", "workspace-write", "danger-full-access":
	default:
		return serveOptions{}, fmt.Errorf("invalid --sandbox %q", options.sandbox)
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
		codexapp.Diagnostic("%v", err)
		return 2
	}
	log, err := protocol.OpenWireLog(options.wireLogDir, "codex-acp")
	if err != nil {
		codexapp.Diagnostic("cannot open wire log: %v", err)
		return 1
	}
	defer log.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client, err := codexapp.Launch(ctx, codexapp.Config{
		Bin:       options.codexBin,
		ExtraArgs: options.codexArgs,
		Dir:       options.cwd,
		Log:       log,
		OptOut:    []string{"thread/readState/changed"},
	})
	if err != nil {
		codexapp.Diagnostic("cannot start codex app-server: %v", err)
		return 1
	}
	server := bridge.New(client, os.Stdin, os.Stdout, bridge.Config{
		ApprovalPolicy:     options.approvalPolicy,
		Sandbox:            options.sandbox,
		Model:              options.model,
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
		codexapp.Diagnostic("ACP server stopped: %v", err)
		return 1
	}
	return 0
}
