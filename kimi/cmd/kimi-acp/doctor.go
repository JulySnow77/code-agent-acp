package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"kimi-acp/acp"
	"kimi-acp/client"
)

// doctor performs a strictly read-only probe: initialize + (if advertised)
// session/list. It never sends session/prompt, never writes files, and by
// default does not create a session.
func runDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	var o commonOpts
	addCommonFlags(fs, &o)
	checkAuth := fs.Bool("check-auth", false, "also call session/new to verify authentication (creates a session)")
	includeTitles := fs.Bool("include-titles", false, "include session titles in the report (off by default for privacy)")
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	trace, err := openTrace(o.trace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "doctor: cannot open trace: %v\n", err)
		return 1
	}
	defer trace.Close()

	type authCheck struct {
		Called    bool          `json:"called"`
		SessionID string        `json:"sessionId,omitempty"`
		Error     *acp.RPCError `json:"error,omitempty"`
	}
	type result struct {
		Tool              string                `json:"tool"`
		Version           string                `json:"version"`
		Command           string                `json:"command"`
		Args              []string              `json:"args"`
		CWD               string                `json:"cwd"`
		Status            string                `json:"status"`
		PromptSent        bool                  `json:"promptSent"`
		ProtocolVersion   int                   `json:"protocolVersion"`
		AgentInfo         *acp.Implementation   `json:"agentInfo,omitempty"`
		AgentCapabilities acp.AgentCapabilities `json:"agentCapabilities"`
		AuthMethods       []acp.AuthMethod      `json:"authMethods"`
		SessionList       []acp.SessionInfo     `json:"sessionList,omitempty"`
		SessionListCalled bool                  `json:"sessionListCalled"`
		AuthCheck         authCheck             `json:"authCheck"`
		StderrTail        []string              `json:"stderrTail"`
		Error             string                `json:"error,omitempty"`
	}

	logger := newLogger()
	copts := client.Options{
		Info:         acp.Implementation{Name: "kimi-acp-go-doctor", Title: "kimi-acp Go doctor", Version: version},
		Capabilities: acp.ClientCapabilities{}, // deliberately no fs / terminal
		ConnOptions: acp.ConnOptions{
			Logger:   logger,
			Observer: trace.observer("doctor"),
		},
	}
	stderrBuf := &syncBuffer{}
	proc, err := startProcess(o, copts, stderrBuf)
	if err != nil {
		fmt.Fprintf(os.Stderr, "doctor: %v\n", err)
		return 1
	}
	defer proc.Close()

	appCtx, stop := appContext()
	defer stop()
	ctx, cancel := context.WithTimeout(appCtx, o.timeout)
	defer cancel()

	out := result{
		Tool:       "kimi-acp doctor",
		Version:    version,
		Command:    o.command,
		Args:       o.args(),
		CWD:        o.cwd,
		Status:     "valid",
		PromptSent: false,
	}
	initResult, err := proc.Client().Initialize(ctx)
	if err != nil {
		out.Status = "initialize_failed"
		out.Error = err.Error()
		out.StderrTail = stderrBuf.Tail(10)
		printJSON(out)
		return 1
	}
	out.ProtocolVersion = initResult.ProtocolVersion
	out.AgentInfo = initResult.AgentInfo
	out.AgentCapabilities = initResult.AgentCapabilities
	out.AuthMethods = initResult.AuthMethods

	if initResult.AgentCapabilities.CanList() {
		if list, err := proc.Client().ListSessions(ctx, acp.ListSessionsParams{}); err == nil {
			out.SessionListCalled = true
			out.SessionList = list.Sessions
			if !*includeTitles {
				for i := range out.SessionList {
					out.SessionList[i].Title = "[redacted]"
				}
			}
		} else {
			out.Error = "session/list: " + err.Error()
		}
	}

	if *checkAuth {
		out.AuthCheck.Called = true
		ns, err := proc.Client().NewSession(ctx, acp.NewSessionParams{CWD: o.cwd})
		if err != nil {
			var rpcErr *acp.RPCError
			if errors.As(err, &rpcErr) {
				out.AuthCheck.Error = rpcErr
			} else {
				out.AuthCheck.Error = acp.Errorf(acp.CodeOther, "%v", err)
			}
		} else if ns != nil {
			out.AuthCheck.SessionID = ns.SessionID
		}
	}

	out.StderrTail = stderrBuf.Tail(10)
	printJSON(out)
	return 0
}
