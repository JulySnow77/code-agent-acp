package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"minimax-acp/acp"
	"minimax-acp/agent"
)

// serve turns this binary into a standard ACP stdio agent. stdout carries ONLY
// ACP frames; all diagnostics go to stderr (or --trace).
func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	name := fs.String("name", "minimax-acp-go-echo", "agentInfo.name advertised to the client")
	tracePath := fs.String("trace", "", "write every ACP frame as ndjson evidence to this file")
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	trace, err := openTrace(*tracePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "serve: cannot open trace: %v\n", err)
		return 1
	}
	defer trace.Close()

	echo := agent.NewEcho()
	opts := agent.Options{
		Info: acp.Implementation{Name: *name, Title: "minimax-acp Go ACP agent", Version: version},
		ConnOptions: acp.ConnOptions{
			Logger:   newLogger(),
			Observer: trace.observer("serve"),
		},
	}
	if err := agent.Serve(context.Background(), acp.NewStdioTransport(), echo.Handlers(), opts); err != nil {
		if !errors.Is(err, io.EOF) {
			fmt.Fprintf(os.Stderr, "serve: %v\n", err)
			return 1
		}
	}
	return 0
}
