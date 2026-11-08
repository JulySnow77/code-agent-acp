// Package cli implements version / sessions / doctor / serve.
package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"workbuddy-acp-bridge/internal/backend"
	"workbuddy-acp-bridge/internal/discovery"
	"workbuddy-acp-bridge/internal/gateway"
	"workbuddy-acp-bridge/internal/protocol"
	"workbuddy-acp-bridge/internal/server"
)

const defaultPermissionMode = "client"

// Main runs the command line and returns the process exit code.
func Main(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	switch args[0] {
	case "version":
		return version(args[1:])
	case "sessions":
		return sessions(args[1:])
	case "doctor":
		return doctor(args[1:])
	case "serve":
		return serve(args[1:])
	case "help", "-h", "--help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: workbuddy-acp-bridge <version|sessions|doctor|serve> [options]")
}

func newFlagSet(name string) *flag.FlagSet {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	return set
}

func version(args []string) int {
	set := newFlagSet("version")
	if err := set.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	fmt.Printf("%s %s\n", protocol.AgentName, protocol.VersionString)
	return 0
}

func sessions(args []string) int {
	set := newFlagSet("sessions")
	asJSON := set.Bool("json", false, "print JSON")
	directory := set.String("sessions-dir", discovery.DefaultSessionsDir(), "session directory")
	if err := set.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	rows := discovery.ReadSessions(*directory)
	if *asJSON {
		output := make([]protocol.Message, 0, len(rows))
		for _, row := range rows {
			output = append(output, protocol.Message{
				"pid": row.Pid, "sessionId": row.SessionID, "cwd": row.Cwd,
				"endpoint": row.Endpoint, "kind": row.Kind,
				"lastHeartbeat": row.LastHeartbeat, "version": row.Version,
			})
		}
		printJSON(output)
		return 0
	}
	for _, row := range rows {
		fmt.Printf("pid=%d  endpoint=%s  session=%s  cwd=%s\n",
			row.Pid, row.Endpoint, row.SessionID, row.Cwd)
	}
	if len(rows) == 0 {
		fmt.Println("no live interactive session found")
	}
	return 0
}

// DoctorReport is the read-only handshake result.
type DoctorReport struct {
	Status            string `json:"status"`
	ProtocolVersion   any    `json:"protocolVersion"`
	Endpoint          any    `json:"endpoint"`
	SessionID         any    `json:"sessionId"`
	AgentCapabilities any    `json:"agentCapabilities"`
	AuthMethods       []any  `json:"authMethods,omitempty"`
	PromptSent        bool   `json:"promptSent"`
	Error             string `json:"error,omitempty"`
}

func doctor(args []string) int {
	set := newFlagSet("doctor")
	endpoint := set.String("endpoint", "", "override the loopback gateway URL")
	sessionID := set.String("session-id", "", "select a session by id")
	cwd := set.String("cwd", "", "select the session whose cwd matches this path")
	directory := set.String("sessions-dir", discovery.DefaultSessionsDir(), "session directory")
	timeoutSeconds := set.Float64("timeout", 10, "gateway timeout in seconds")
	asJSON := set.Bool("json", false, "print JSON")
	if err := set.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	report := DoctorReport{Status: "unknown", PromptSent: false}
	target, err := discovery.Select(discovery.Options{
		SessionID: *sessionID, Cwd: *cwd, Endpoint: *endpoint, Directory: *directory,
	})
	if err == nil {
		report.Endpoint = target.Endpoint
		if target.SessionID != "" {
			report.SessionID = target.SessionID
		}
		client, createErr := gateway.New(target.Endpoint, seconds(*timeoutSeconds))
		if createErr != nil {
			err = createErr
		} else {
			var result protocol.Message
			result, err = client.Initialize()
			if err == nil {
				report.Status = "valid"
				report.ProtocolVersion = result["protocolVersion"]
				report.AgentCapabilities = result["agentCapabilities"]
				if methods, ok := result["authMethods"].([]any); ok {
					ids := make([]any, 0, len(methods))
					for _, method := range methods {
						if row, ok := method.(map[string]any); ok {
							if id, ok := row["id"]; ok {
								ids = append(ids, id)
							}
						}
					}
					report.AuthMethods = ids
				}
			}
		}
	}
	if err != nil {
		report.Status = "error"
		report.Error = err.Error()
	}
	if *asJSON {
		printJSON(report)
	} else {
		printDoctorText(report)
	}
	if report.Status == "valid" {
		return 0
	}
	return 1
}

type doctorPair struct {
	key   string
	value any
}

func printDoctorText(report DoctorReport) {
	pairs := []doctorPair{
		{"status", report.Status},
		{"protocolVersion", report.ProtocolVersion},
		{"endpoint", report.Endpoint},
		{"sessionId", report.SessionID},
		{"agentCapabilities", report.AgentCapabilities},
		{"promptSent", report.PromptSent},
	}
	if report.AuthMethods != nil {
		pairs = append(pairs, doctorPair{"authMethods", report.AuthMethods})
	}
	if report.Error != "" {
		pairs = append(pairs, doctorPair{"error", report.Error})
	}
	for _, pair := range pairs {
		encoded, _ := protocol.Encode(pair.value)
		fmt.Printf("%s: %s\n", pair.key, encoded)
	}
	fmt.Println("doctor is read-only: no session/load and no session/prompt were sent; " +
		"a live WorkBuddy session may still be occupied by another writer.")
}

func serve(args []string) int {
	set := newFlagSet("serve")
	mode := set.String("mode", "build", "plan or build")
	backendName := set.String("backend", "local", "local or fake")
	endpoint := set.String("endpoint", "", "override the loopback gateway URL")
	sessionID := set.String("session-id", "", "bind to a specific WorkBuddy session")
	cwd := set.String("cwd", "", "select the session whose cwd matches this path")
	timeoutSeconds := set.Float64("timeout", 1800, "gateway timeout in seconds")
	permissionMode := set.String("permission-mode", defaultPermissionMode, "deny or client")
	fakeResult := set.String("fake-result", "", "JSON result returned by the fake backend")
	if err := set.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if *mode != "plan" && *mode != "build" {
		backend.Log("cannot start: unknown mode %q", *mode)
		return 1
	}
	// Hosts such as auto-run start the bridge with fixed argv
	// (`serve --mode plan|build`) and cannot pass extra flags.
	if override := os.Getenv("WORKBUDDY_ACP_BACKEND"); override != "" {
		*backendName = override
	}
	if override := os.Getenv("WORKBUDDY_ACP_PERMISSION_MODE"); override != "" {
		*permissionMode = override
	}
	if *backendName != "local" && *backendName != "fake" {
		backend.Log("cannot start: unknown WORKBUDDY_ACP_BACKEND=%q", *backendName)
		return 1
	}
	if *permissionMode != "deny" && *permissionMode != "client" {
		backend.Log("cannot start: unknown permission mode %q", *permissionMode)
		return 1
	}
	var bridge backend.Backend
	var err error
	if *backendName == "fake" {
		if *fakeResult != "" {
			_ = os.Setenv("WORKBUDDY_FAKE_RESULT", *fakeResult)
		}
		bridge, err = backend.NewFake(nil, nil)
	} else {
		bridge, err = backend.NewLocal(backend.LocalOptions{
			Endpoint: *endpoint, SessionID: *sessionID, Cwd: *cwd,
			Timeout: seconds(*timeoutSeconds),
		})
	}
	if err != nil {
		backend.Log("cannot start: %v", err)
		return 1
	}
	bridge.SetMode(*mode)
	backend.Log("serve backend=%s mode=%s permission=%s endpoint=%s",
		bridge.Name(), *mode, *permissionMode, emptyAsNA(bridge.Endpoint()))
	return server.New(bridge, os.Stdin, os.Stdout, *permissionMode).Run()
}

func emptyAsNA(value string) string {
	if value == "" {
		return "n/a"
	}
	return value
}

func seconds(value float64) time.Duration {
	if value <= 0 {
		value = 10
	}
	return time.Duration(value * float64(time.Second))
}

func printJSON(value any) {
	raw, err := protocol.Encode(value)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, raw, "", "  "); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	fmt.Println(indented.String())
}
