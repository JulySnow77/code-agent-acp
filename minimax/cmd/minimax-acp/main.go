// minimax-acp is a zero-dependency Go implementation of the Agent Client
// Protocol (ACP v1). It can drive `mcode acp` as a client, serve ACP as an
// agent, or transparently bridge the two.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"minimax-acp/acp"
	"minimax-acp/client"
	"minimax-acp/internal/minimax"
)

const version = "00.00.01"

type commonOpts struct {
	command string
	argsRaw string
	cwd     string
	trace   string
	timeout time.Duration
}

func addCommonFlags(fs *flag.FlagSet, o *commonOpts) {
	wd, _ := os.Getwd()
	fs.StringVar(&o.command, "command", minimax.CommandFromEnv(), "agent executable (override with MINIMAX_ACP_CLI)")
	fs.StringVar(&o.argsRaw, "args", minimax.DefaultArgs, "comma-separated agent arguments (native ACP server: acp)")
	fs.StringVar(&o.cwd, "cwd", wd, "absolute working directory for sessions")
	fs.StringVar(&o.trace, "trace", "", "write every ACP frame as ndjson evidence to this file")
	fs.DurationVar(&o.timeout, "timeout", 5*time.Minute, "per-request timeout")
}

func (o commonOpts) args() []string {
	if o.argsRaw == "" {
		return []string{}
	}
	parts := strings.Split(o.argsRaw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func newLogger() *log.Logger { return log.New(os.Stderr, "minimax-acp: ", 0) }

func startProcess(o commonOpts, copts client.Options, stderrWriter io.Writer) (*client.AgentProcess, error) {
	return client.Start(client.ProcessConfig{
		Command:       o.command,
		Args:          o.args(),
		Dir:           o.cwd,
		Stderr:        stderrWriter,
		ClientOptions: copts,
	})
}

func appContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// syncBuffer captures child stderr without racing with the read loop.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) Tail(n int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	lines := strings.Split(strings.TrimSpace(s.b.String()), "\n")
	out := make([]string, 0, n)
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// traceRecorder writes frames as ndjson. It never writes to stdout.
type traceRecorder struct {
	mu sync.Mutex
	f  *os.File
	w  *bufio.Writer
}

func openTrace(path string) (*traceRecorder, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	return &traceRecorder{f: f, w: bufio.NewWriter(f)}, nil
}

func (t *traceRecorder) observer(side string) func(acp.FrameDirection, []byte) {
	if t == nil {
		return nil
	}
	return func(dir acp.FrameDirection, frame []byte) {
		entry := struct {
			Time  string             `json:"time"`
			Side  string             `json:"side"`
			Dir   acp.FrameDirection `json:"dir"`
			Frame json.RawMessage    `json:"frame"`
		}{Time: time.Now().UTC().Format(time.RFC3339Nano), Side: side, Dir: dir, Frame: append(json.RawMessage(nil), frame...)}
		b, err := json.Marshal(entry)
		if err != nil {
			return
		}
		t.mu.Lock()
		defer t.mu.Unlock()
		_, _ = t.w.Write(append(b, '\n'))
		_ = t.w.Flush()
	}
}

func (t *traceRecorder) Close() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.w.Flush(); err != nil {
		_ = t.f.Close()
		return err
	}
	return t.f.Close()
}

func usage() {
	fmt.Fprintf(os.Stderr, `minimax-acp %s - ACP v1 (JSON-RPC 2.0 / stdio) in Go

Usage:
  minimax-acp doctor [flags]    read-only probe: CLI/env check + initialize (+ session/list)
  minimax-acp chat   [flags]    drive a native mcode acp session, streaming updates
  minimax-acp serve  [flags]    run a deterministic Go ACP agent over stdio
  minimax-acp bridge [flags]    transparent ACP stdio proxy in front of mcode acp
  minimax-acp version           print version

doctor never sends session/prompt; --check-auth is the only opt-in side effect.
Common flags: --command, --args, --cwd, --trace, --timeout
MINIMAX_ACP_CLI overrides the CLI path for hosts that cannot pass flags.
Native MiniMax Code ACP config: {"command": "mcode", "args": ["acp"]}
Run "minimax-acp <command> -h" for command-specific flags.
`, version)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var code int
	switch os.Args[1] {
	case "version", "--version", "-v":
		fmt.Printf("minimax-acp %s\n", version)
	case "doctor":
		code = runDoctor(os.Args[2:])
	case "chat":
		code = runChat(os.Args[2:])
	case "serve":
		code = runServe(os.Args[2:])
	case "bridge":
		code = runBridge(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "minimax-acp: unknown command %q\n", os.Args[1])
		usage()
		code = 2
	}
	os.Exit(code)
}

func printJSON(v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot encode JSON: %v\n", err)
		return
	}
	fmt.Println(string(b))
}
