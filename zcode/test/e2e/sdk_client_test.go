package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// sdkCallbacks deliberately advertises no filesystem or terminal support.
// Tests use the real SDK dispatcher, validation, and cancellation contexts.
type sdkCallbacks struct {
	permission func(context.Context, acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error)
	form       func(context.Context, acp.UnstableCreateElicitationRequest) (acp.UnstableCreateElicitationResponse, error)
}

func (c *sdkCallbacks) SessionUpdate(context.Context, acp.SessionNotification) error { return nil }
func (c *sdkCallbacks) RequestPermission(ctx context.Context, p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	if c.permission != nil {
		return c.permission(ctx, p)
	}
	return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Cancelled: &acp.RequestPermissionOutcomeCancelled{}}}, nil
}
func (c *sdkCallbacks) UnstableCreateElicitation(ctx context.Context, p acp.UnstableCreateElicitationRequest) (acp.UnstableCreateElicitationResponse, error) {
	if c.form != nil {
		return c.form(ctx, p)
	}
	return acp.UnstableCreateElicitationResponse{Cancel: &acp.UnstableCreateElicitationCancel{}}, nil
}
func (*sdkCallbacks) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, acp.NewMethodNotFound("ReadTextFile")
}
func (*sdkCallbacks) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, acp.NewMethodNotFound("WriteTextFile")
}
func (*sdkCallbacks) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, acp.NewMethodNotFound("CreateTerminal")
}
func (*sdkCallbacks) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, acp.NewMethodNotFound("KillTerminal")
}
func (*sdkCallbacks) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, acp.NewMethodNotFound("TerminalOutput")
}
func (*sdkCallbacks) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, acp.NewMethodNotFound("ReleaseTerminal")
}
func (*sdkCallbacks) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, acp.NewMethodNotFound("WaitForTerminalExit")
}

var _ acp.Client = (*sdkCallbacks)(nil)

type sdkHarness struct {
	conn  *acp.ClientSideConnection
	stdin io.WriteCloser
	exit  chan error
	log   string
	ctx   context.Context
	sid   acp.SessionId
	cwd   string
}

func startSDK(t *testing.T, script fakeScript, callbacks *sdkCallbacks, env map[string]string) *sdkHarness {
	t.Helper()
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "script.json")
	provider := filepath.Join(dir, "providers.json")
	raw, err := json.Marshal(script)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{scriptPath: raw, provider: []byte(`{"providers":{}}`)} {
		if err := os.WriteFile(name, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	fakePath := filepath.Join(dir, "fakeserver")
	if out, err := exec.Command("go", "build", "-o", fakePath, "github.com/tizerluo/zcode-open-bridge-go/test/fakeserver").CombinedOutput(); err != nil {
		t.Fatalf("build fake: %v\n%s", err, out)
	}
	h := &sdkHarness{exit: make(chan error, 1), log: filepath.Join(dir, "requests.jsonl"), cwd: dir}
	cmd := exec.Command(bridgeBin, "serve", "--mode", "build")
	// Keep real provider credentials and unrelated ZCode settings out of this fake test.
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(key, "ZCODE_") && !strings.HasPrefix(key, "FAKE_ZCODE_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "ZCODE_BIN="+fakePath, "ZCODE_BUILTIN_PROVIDER_CONFIG_FILE="+provider, "FAKE_ZCODE_SCRIPT="+scriptPath, "FAKE_ZCODE_LOG="+h.log)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.Create(filepath.Join(dir, "stderr.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		stderr.Close()
		t.Fatal(err)
	}
	h.stdin = stdin
	h.conn = acp.NewClientSideConnection(callbacks, stdin, stdout)
	go func() { h.exit <- cmd.Wait() }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	h.ctx = ctx
	t.Cleanup(func() {
		cancel()
		stdin.Close()
		select {
		case <-h.exit:
		case <-time.After(3 * time.Second):
			cmd.Process.Kill()
			<-h.exit
		}
		stdout.Close()
		stderr.Close()
		if t.Failed() {
			raw, _ := os.ReadFile(stderr.Name())
			t.Logf("bridge stderr: %s", raw)
		}
	})
	_, err = h.conn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: 1,
		ClientCapabilities: acp.ClientCapabilities{
			Meta:        map[string]any{"_zcode": map[string]any{"cancelRequest": true}},
			Elicitation: &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := h.conn.NewSession(ctx, acp.NewSessionRequest{Cwd: dir, McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatal(err)
	}
	h.sid = session.SessionId
	return h
}

func (h *sdkHarness) prompt() <-chan error {
	done := make(chan error, 1)
	go func() {
		resp, err := h.conn.Prompt(h.ctx, acp.PromptRequest{SessionId: h.sid, Prompt: []acp.ContentBlock{acp.TextBlock("contract test")}})
		if err == nil && resp.StopReason != acp.StopReasonEndTurn && resp.StopReason != acp.StopReasonCancelled {
			err = fmt.Errorf("unexpected stop reason: %s", resp.StopReason)
		}
		done <- err
	}()
	return done
}
func awaitSDK(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("SDK prompt did not finish")
	}
}
func awaitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal(what)
	}
}
func (h *sdkHarness) entries(t *testing.T) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(h.log)
	if err != nil {
		t.Fatal(err)
	}
	var entries []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
	}
	return entries
}
func (h *sdkHarness) reply(t *testing.T) map[string]any {
	t.Helper()
	for _, entry := range h.entries(t) {
		if reply, ok := entry["reply"].(map[string]any); ok {
			result, _ := reply["result"].(map[string]any)
			return result
		}
	}
	t.Fatal("fake backend never received a callback result")
	return nil
}

func TestSDKInteractionCancellation(t *testing.T) {
	for _, kind := range []string{"permission", "form"} {
		for _, trigger := range []string{"cancel", "timeout", "eof"} {
			t.Run(kind+"/"+trigger, func(t *testing.T) {
				entered, finished := make(chan struct{}), make(chan struct{})
				wait := func(ctx context.Context) { close(entered); <-ctx.Done(); close(finished) }
				cb := &sdkCallbacks{}
				script := askScript(frameworkQuestion)
				if kind == "permission" {
					script = permScript(zcodePermissionOptions())
					cb.permission = func(ctx context.Context, _ acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
						wait(ctx)
						return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Selected: &acp.RequestPermissionOutcomeSelected{OptionId: "allow_once"}}}, nil
					}
				} else {
					cb.form = func(ctx context.Context, _ acp.UnstableCreateElicitationRequest) (acp.UnstableCreateElicitationResponse, error) {
						wait(ctx)
						return acp.UnstableCreateElicitationResponse{Accept: &acp.UnstableCreateElicitationAccept{Content: map[string]any{"Which framework should we use?": "vue"}}}, nil
					}
				}
				script.Responses["session/stop"] = fakeAction{Result: map[string]any{}}
				env := map[string]string{}
				if trigger == "timeout" {
					env["ZCODE_ACP_QUESTION_TIMEOUT"] = "0.3"
				}
				h := startSDK(t, script, cb, env)
				done := h.prompt()
				awaitSignal(t, entered, "SDK callback was not entered")
				switch trigger {
				case "cancel":
					if err := h.conn.Cancel(h.ctx, acp.CancelNotification{SessionId: h.sid}); err != nil {
						t.Fatal(err)
					}
				case "eof":
					if err := h.stdin.Close(); err != nil {
						t.Fatal(err)
					}
				}
				awaitSignal(t, finished, "abandoned SDK callback was not cancelled")
				if trigger != "eof" {
					awaitSDK(t, done)
					result := h.reply(t)
					if kind == "permission" && result["decision"] != "deny" {
						t.Fatalf("late approval escaped cancellation: %v", result)
					}
					if kind == "form" && result["action"] != "cancel" {
						t.Fatalf("late form escaped cancellation: %v", result)
					}
				} else {
					select {
					case err := <-h.exit:
						h.exit <- err
						if err != nil {
							t.Fatal(err)
						}
					case <-time.After(3 * time.Second):
						t.Fatal("bridge failed to exit after EOF")
					}
				}
				if trigger == "cancel" || trigger == "eof" {
					stopped := false
					for _, entry := range h.entries(t) {
						if entry["method"] == "session/stop" {
							stopped = true
						}
					}
					if !stopped {
						t.Fatal("abandoned turn was not stopped in backend")
					}
				}
			})
		}
	}
}

func TestSDKFormValues(t *testing.T) {
	for _, tc := range []struct {
		name      string
		questions []map[string]any
		answer    map[string]any
		want      any
	}{
		{"plan", []map[string]any{planApprovalQuestion}, map[string]any{"Review this implementation plan.": "approve"}, "approve"},
		{"multiselect_empty", []map[string]any{platformsQuestion}, map[string]any{"Which platforms must be supported?[ios]": false, "Which platforms must be supported?[android]": false}, []any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cb := &sdkCallbacks{form: func(_ context.Context, p acp.UnstableCreateElicitationRequest) (acp.UnstableCreateElicitationResponse, error) {
				if p.Form == nil {
					return acp.UnstableCreateElicitationResponse{}, fmt.Errorf("missing form")
				}
				expiry, _ := p.Form.Meta["_zcode"].(map[string]any)
				expiresAt, _ := expiry["expiresAt"].(string)
				if _, err := time.Parse(time.RFC3339Nano, expiresAt); err != nil {
					return acp.UnstableCreateElicitationResponse{}, fmt.Errorf("missing expiry: %v", expiry)
				}
				if p.Form.Meta["zcode"] == nil {
					return acp.UnstableCreateElicitationResponse{}, fmt.Errorf("SDK discarded question metadata")
				}
				return acp.UnstableCreateElicitationResponse{Accept: &acp.UnstableCreateElicitationAccept{Content: tc.answer}}, nil
			}}
			h := startSDK(t, askScript(tc.questions...), cb, nil)
			awaitSDK(t, h.prompt())
			result := h.reply(t)
			answers := askAnswers(t, result)
			got, _ := json.Marshal(answers[tc.questions[0]["question"].(string)])
			want, _ := json.Marshal(tc.want)
			if string(got) != string(want) {
				t.Fatalf("backend answer = %s, want %s", got, want)
			}
		})
	}
}

func TestSDKPermissionRoundTrip(t *testing.T) {
	cb := &sdkCallbacks{permission: func(_ context.Context, p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
		for _, opt := range p.Options {
			if opt.OptionId == "allow_once" {
				return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Selected: &acp.RequestPermissionOutcomeSelected{OptionId: opt.OptionId}}}, nil
			}
		}
		return acp.RequestPermissionResponse{}, fmt.Errorf("allow_once missing")
	}}
	h := startSDK(t, permScript(zcodePermissionOptions()), cb, nil)
	awaitSDK(t, h.prompt())
	if got := h.reply(t)["decision"]; got != "allow" {
		t.Fatalf("backend decision = %v", got)
	}
}

func TestSDKSessionLifecycle(t *testing.T) {
	switched := fakeSettings("plan")
	switched["model"] = fakeModelSettings("prov-2", "gpt-5")
	script := fakeScript{Responses: map[string]fakeAction{
		"session/create":    {Result: map[string]any{"session": map[string]any{"sessionId": "sdk-session"}, "settings": fakeSettings("build")}},
		"session/resume":    {Result: map[string]any{"session": map[string]any{"sessionId": "sdk-session"}, "settings": switched}},
		"session/setMode":   {Result: map[string]any{}},
		"session/setModel":  {Result: map[string]any{"settings": switched}},
		"session/subscribe": {Result: map[string]any{}},
		"session/send":      {Result: map[string]any{"accepted": true}, Push: []fakePush{{Method: "session/event", Params: map[string]any{"sessionId": "sdk-session", "type": "turn.completed", "payload": map[string]any{"resultType": "success"}}}}},
		"session/messages":  {Result: map[string]any{"messages": []any{}}},
	}}
	h := startSDK(t, script, &sdkCallbacks{}, nil)
	if h.sid != "sdk-session" {
		t.Fatalf("session id = %s", h.sid)
	}
	if _, err := h.conn.SetSessionMode(h.ctx, acp.SetSessionModeRequest{SessionId: h.sid, ModeId: "plan"}); err != nil {
		t.Fatal(err)
	}
	config, err := h.conn.SetSessionConfigOption(h.ctx, acp.SetSessionConfigOptionRequest{ValueId: &acp.SetSessionConfigOptionValueId{SessionId: h.sid, ConfigId: "model", Value: "prov-2/gpt-5"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(config.ConfigOptions) != 1 || config.ConfigOptions[0].Select == nil || config.ConfigOptions[0].Select.CurrentValue != "prov-2/gpt-5" {
		t.Fatalf("model readback: %+v", config.ConfigOptions)
	}
	awaitSDK(t, h.prompt())
	resumed, err := h.conn.ResumeSession(h.ctx, acp.ResumeSessionRequest{SessionId: h.sid, Cwd: h.cwd})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Modes == nil || resumed.Modes.CurrentModeId != "plan" {
		t.Fatalf("resume modes: %+v", resumed.Modes)
	}
	awaitSDK(t, h.prompt())
	var sends int
	for _, entry := range h.entries(t) {
		params, _ := entry["params"].(map[string]any)
		if entry["method"] == "session/send" {
			sends++
			if params["sessionId"] != string(h.sid) {
				t.Fatalf("wrong session: %v", params)
			}
		}
		if entry["method"] == "session/create" || entry["method"] == "session/resume" {
			ws, _ := params["workspace"].(map[string]any)
			if ws["workspacePath"] != h.cwd {
				t.Fatalf("cwd was not preserved: %v", params)
			}
			if entry["method"] == "session/create" && params["mode"] != "build" {
				t.Fatalf("unsafe default mode: %v", params)
			}
		}
	}
	if sends != 2 {
		t.Fatalf("send count %d, want 2", sends)
	}
}

func TestSDKPromptFailures(t *testing.T) {
	for _, kind := range []string{"backend_error", "turn_timeout"} {
		t.Run(kind, func(t *testing.T) {
			script := askScript(frameworkQuestion)
			action := fakeAction{Result: map[string]any{"accepted": true}}
			env := map[string]string{}
			if kind == "backend_error" {
				action.Error = &fakeRPCError{Code: -32000, Message: "test send failure"}
			} else {
				env["ZCODE_ACP_TURN_TIMEOUT"] = "0.1"
			}
			script.Responses["session/send"] = action
			script.Responses["session/stop"] = fakeAction{Result: map[string]any{}}
			h := startSDK(t, script, &sdkCallbacks{}, env)
			select {
			case err := <-h.prompt():
				if err == nil {
					t.Fatal("failure became successful prompt")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("failed prompt hung")
			}
			if kind == "turn_timeout" {
				stopped := false
				for _, entry := range h.entries(t) {
					if entry["method"] == "session/stop" {
						stopped = true
					}
				}
				if !stopped {
					t.Fatal("timed out backend not stopped")
				}
			}
		})
	}
}

func TestSDKPlanApprovalThenPermission(t *testing.T) {
	script := askScript(planApprovalQuestion)
	send := script.Responses["session/send"]
	send.ServerRequests[0].Then = nil
	permission := permScript(zcodePermissionOptions()).Responses["session/send"].ServerRequests[0]
	permission.Params["sessionId"] = "s-ask"
	permission.Then[0].Params["sessionId"] = "s-ask"
	send.ServerRequests = append(send.ServerRequests, permission)
	script.Responses["session/send"] = send
	cb := &sdkCallbacks{
		form: func(_ context.Context, p acp.UnstableCreateElicitationRequest) (acp.UnstableCreateElicitationResponse, error) {
			if p.Form == nil {
				return acp.UnstableCreateElicitationResponse{}, fmt.Errorf("expected plan form")
			}
			return acp.UnstableCreateElicitationResponse{Accept: &acp.UnstableCreateElicitationAccept{Content: map[string]any{"Review this implementation plan.": "approve"}}}, nil
		},
		permission: func(_ context.Context, p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
			if p.SessionId != "s-ask" {
				return acp.RequestPermissionResponse{}, fmt.Errorf("permission bound to wrong session: %s", p.SessionId)
			}
			return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Selected: &acp.RequestPermissionOutcomeSelected{OptionId: "allow_once"}}}, nil
		},
	}
	h := startSDK(t, script, cb, nil)
	awaitSDK(t, h.prompt())
	var approved, allowed bool
	for _, entry := range h.entries(t) {
		reply, _ := entry["reply"].(map[string]any)
		result, _ := reply["result"].(map[string]any)
		if result["action"] == "accept" {
			approved = askAnswers(t, result)["Review this implementation plan."] == "approve"
		}
		if result["decision"] == "allow" {
			allowed = true
		}
	}
	if !approved || !allowed {
		t.Fatalf("incomplete approval chain: plan=%v permission=%v", approved, allowed)
	}
}

func TestSDKModelFailureIsVisible(t *testing.T) {
	h := startSDK(t, fakeScript{Responses: map[string]fakeAction{
		"session/create":   {Result: map[string]any{"session": map[string]any{"sessionId": "sdk-model"}, "settings": fakeSettings("build")}},
		"session/setModel": {Error: &fakeRPCError{Code: -32000, Message: "model unavailable"}},
	}}, &sdkCallbacks{}, nil)
	for _, model := range []acp.SessionConfigValueId{"no-such/model", "prov-2/gpt-5"} {
		_, err := h.conn.SetSessionConfigOption(h.ctx, acp.SetSessionConfigOptionRequest{ValueId: &acp.SetSessionConfigOptionValueId{SessionId: h.sid, ConfigId: "model", Value: model}})
		if err == nil {
			t.Fatalf("model %q failure became success", model)
		}
	}
	var calls int
	for _, entry := range h.entries(t) {
		if entry["method"] == "session/setModel" {
			calls++
		}
		if entry["method"] == "session/send" {
			t.Fatal("model failure sent a prompt")
		}
	}
	if calls != 1 {
		t.Fatalf("unknown model reached backend: %d calls", calls)
	}
}
