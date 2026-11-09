// kimi-acp is a zero-dependency Go implementation of the Agent Client
// Protocol (ACP v1). It can drive `kimi acp` as a client, serve ACP as an
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

	"kimi-acp/acp"
	"kimi-acp/client"
)

const version = "00.00.02"

type commonOpts struct {
	command string
	argsRaw string
	cwd     string
	trace   string
	timeout time.Duration
}

func addCommonFlags(fs *flag.FlagSet, o *commonOpts) {
	wd, _ := os.Getwd()
	fs.StringVar(&o.command, "command", "kimi", "agent executable")
	fs.StringVar(&o.argsRaw, "args", "acp", "comma-separated agent arguments (default: acp)")
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

func newLogger() *log.Logger { return log.New(os.Stderr, "kimi-acp: ", 0) }

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
	fmt.Fprintf(os.Stderr, `kimi-acp %s - ACP v1 (JSON-RPC 2.0 / stdio) in Go

Usage:
  kimi-acp doctor [flags]    read-only handshake against kimi acp (initialize + session/list)
  kimi-acp chat   [flags]    drive a kimi acp session, streaming updates
  kimi-acp serve  [flags]    run a Go ACP agent over stdio (for Zed / JetBrains)
  kimi-acp bridge [flags]    transparent ACP stdio proxy in front of kimi acp
  kimi-acp version           print version

Common flags: --command, --args, --cwd, --trace, --timeout
Run "kimi-acp <command> -h" for command-specific flags.
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
		fmt.Printf("kimi-acp %s\n", version)
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
		fmt.Fprintf(os.Stderr, "kimi-acp: unknown command %q\n", os.Args[1])
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
