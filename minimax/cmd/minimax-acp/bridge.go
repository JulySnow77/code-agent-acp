package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"time"

	"minimax-acp/acp"
	"minimax-acp/bridge"
)

// bridge is the Go counterpart of an ACP adapter: an ACP client (editor) talks
// ACP v1 stdio to us, we forward every frame to a real `mcode acp` subprocess.
func runBridge(args []string) int {
	fs := flag.NewFlagSet("bridge", flag.ContinueOnError)
	var o commonOpts
	addCommonFlags(fs, &o)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	trace, err := openTrace(o.trace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bridge: cannot open trace: %v\n", err)
		return 1
	}
	defer trace.Close()

	cmd := exec.Command(o.command, o.args()...)
	if o.cwd != "" {
		cmd.Dir = o.cwd
	}
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "bridge: stdin pipe: %v\n", err)
		return 1
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		fmt.Fprintf(os.Stderr, "bridge: stdout pipe: %v\n", err)
		return 1
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		fmt.Fprintf(os.Stderr, "bridge: start %s %v: %v\n", o.command, o.args(), err)
		return 1
	}
	upstream := acp.NewLineTransport(stdout, stdin, stdin)
	downstream := acp.NewStdioTransport()

	b := bridge.New(downstream, upstream, bridge.Options{
		Logger:             newLogger(),
		DownstreamObserver: trace.observer("down"),
		UpstreamObserver:   trace.observer("up"),
	})
	appCtx, stop := appContext()
	defer stop()
	runErr := b.Run(appCtx)
	b.Close()

	// Let the child observe EOF on stdin and exit; kill it if it lingers.
	_ = upstream.Close()
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	select {
	case <-waitCh:
	case <-time.After(3 * time.Second):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-waitCh
	}
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "bridge: %v\n", runErr)
		return 1
	}
	return 0
}
