package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"minimax-acp/acp"
	"minimax-acp/client"
	"minimax-acp/internal/minimax"
)

type doctorAuthCheck struct {
	Called    bool          `json:"called"`
	SessionID string        `json:"sessionId,omitempty"`
	Error     *acp.RPCError `json:"error,omitempty"`
}

type doctorReport struct {
	Tool               string                `json:"tool"`
	Version            string                `json:"version"`
	Command            string                `json:"command"`
	Args               []string              `json:"args"`
	CWD                string                `json:"cwd"`
	Status             string                `json:"status"`
	PromptSent         bool                  `json:"promptSent"`
	SessionCreated     bool                  `json:"sessionCreated"`
	Region             string                `json:"region,omitempty"`
	HandshakeTimeoutMS int64                 `json:"handshakeTimeoutMs"`
	CLI                minimax.Environment   `json:"cli"`
	ProtocolVersion    int                   `json:"protocolVersion"`
	AgentInfo          *acp.Implementation   `json:"agentInfo,omitempty"`
	AgentCapabilities  acp.AgentCapabilities `json:"agentCapabilities"`
	AuthMethods        []acp.AuthMethod      `json:"authMethods"`
	SessionList        []acp.SessionInfo     `json:"sessionList,omitempty"`
	SessionListCalled  bool                  `json:"sessionListCalled"`
	AuthCheck          doctorAuthCheck       `json:"authCheck"`
	StderrTail         []string              `json:"stderrTail"`
	Diagnostics        []string              `json:"diagnostics,omitempty"`
	Error              string                `json:"error,omitempty"`
}

// doctor performs a strictly read-only probe by default:
//
//   - inspect the CLI and host environment (never reads credential contents),
//   - send `initialize`,
//   - call `session/list` only when the agent advertises it.
//
// It never sends session/prompt and never creates a session unless
// --check-auth is passed explicitly (that flag has a side effect and is off
// by default). A bounded handshake timeout prevents a silent agent from
// hanging forever: under some desktop sandboxes `mcode acp` produces no
// stdio frame at all.
func runDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	var o commonOpts
	addCommonFlags(fs, &o)
	jsonOut := fs.Bool("json", true, "emit a JSON report (false prints a short human-readable summary)")
	region := fs.String("region", "", "account region hint used in the login command (for example: global)")
	handshakeTimeout := fs.Duration("handshake-timeout", 30*time.Second, "timeout for the read-only initialize handshake")
	checkAuth := fs.Bool("check-auth", false, "also call session/new (SIDE EFFECT: creates a session; off by default)")
	includeTitles := fs.Bool("include-titles", false, "include session titles in the report (off by default for privacy)")
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *handshakeTimeout <= 0 {
		fmt.Fprintln(os.Stderr, "doctor: --handshake-timeout must be positive")
		return 2
	}

	trace, err := openTrace(o.trace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "doctor: cannot open trace: %v\n", err)
		return 1
	}
	defer trace.Close()

	env := minimax.New().Inspect(o.command, *region)
	logger := newLogger()
	copts := client.Options{
		Info:         acp.Implementation{Name: "minimax-acp-go-doctor", Title: "minimax-acp Go doctor", Version: version},
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
	ctx, cancel := context.WithTimeout(appCtx, *handshakeTimeout)
	defer cancel()

	out := doctorReport{
		Tool:               "minimax-acp doctor",
		Version:            version,
		Command:            o.command,
		Args:               o.args(),
		CWD:                o.cwd,
		Status:             "valid",
		PromptSent:         false,
		Region:             strings.TrimSpace(*region),
		HandshakeTimeoutMS: handshakeTimeout.Milliseconds(),
		CLI:                env,
	}
	initResult, err := proc.Client().Initialize(ctx)
	if err != nil {
		out.Status = "initialize_failed"
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			out.Status = "no_response"
			out.Diagnostics = append(out.Diagnostics,
				"在握手超时内未收到 initialize 响应；mcode acp 可能未进入协议循环",
				"请在普通终端（非 IDE 沙箱）里重试，并核对 cli.warnings")
		}
		out.Error = err.Error()
		out.Diagnostics = append(out.Diagnostics, env.Warnings...)
		out.StderrTail = stderrBuf.Tail(10)
		emitDoctorReport(out, *jsonOut)
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
			out.Diagnostics = append(out.Diagnostics, "session/list: "+err.Error())
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
			out.SessionCreated = true
		}
	}

	out.Diagnostics = append(out.Diagnostics, env.Warnings...)
	out.StderrTail = stderrBuf.Tail(10)
	emitDoctorReport(out, *jsonOut)
	return 0
}

func emitDoctorReport(out doctorReport, jsonOut bool) {
	if jsonOut {
		printJSON(out)
		return
	}
	fmt.Printf("status: %s\n", out.Status)
	fmt.Printf("cli: %s %s (native ACP command: %v, login state dir: %v)\n",
		out.CLI.Command, out.CLI.CLIVersion, out.CLI.ACPServerHelp, out.CLI.AuthDirExists)
	fmt.Printf("protocolVersion: %d\n", out.ProtocolVersion)
	if out.AgentInfo != nil {
		fmt.Printf("agent: %s %s\n", out.AgentInfo.Name, out.AgentInfo.Version)
	}
	fmt.Printf("capabilities: loadSession=%v list=%v resume=%v authMethods=%d\n",
		out.AgentCapabilities.CanLoadSession(), out.AgentCapabilities.CanList(),
		out.AgentCapabilities.CanResume(), len(out.AuthMethods))
	fmt.Printf("promptSent=%v sessionCreated=%v sessionListCalled=%v\n",
		out.PromptSent, out.SessionCreated, out.SessionListCalled)
	if out.Error != "" {
		fmt.Printf("error: %s\n", out.Error)
	}
	for _, w := range out.Diagnostics {
		fmt.Printf("- %s\n", w)
	}
}
