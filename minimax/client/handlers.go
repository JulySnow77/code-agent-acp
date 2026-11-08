package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"minimax-acp/acp"
)

// PermissionMode controls the default answer to session/request_permission.
type PermissionMode string

const (
	PermissionDeny  PermissionMode = "deny"
	PermissionAllow PermissionMode = "allow"
)

// AutoPermission answers without user interaction. deny picks a reject option
// (or "cancelled" when none exists); allow picks an allow option. It never
// treats an unknown outcome as approval.
func AutoPermission(mode PermissionMode) func(context.Context, *acp.RequestPermissionParams) (*acp.RequestPermissionResult, *acp.RPCError) {
	return func(_ context.Context, p *acp.RequestPermissionParams) (*acp.RequestPermissionResult, *acp.RPCError) {
		if p == nil || len(p.Options) == 0 {
			return ptrResult(acp.CancelledOutcome()), nil
		}
		wantAllow := mode == PermissionAllow
		firstAllow, firstReject := "", ""
		for _, o := range p.Options {
			allow := strings.HasPrefix(o.Kind, "allow")
			if allow && firstAllow == "" {
				firstAllow = o.OptionID
			}
			if !allow && firstReject == "" {
				firstReject = o.OptionID
			}
			// Prefer the least-privileged variant of the desired decision.
			if allow && o.Kind == acp.PermissionAllowOnce && wantAllow {
				return ptrResult(acp.SelectedOutcome(o.OptionID)), nil
			}
			if !allow && o.Kind == acp.PermissionRejectOnce && !wantAllow {
				return ptrResult(acp.SelectedOutcome(o.OptionID)), nil
			}
		}
		if wantAllow && firstAllow != "" {
			return ptrResult(acp.SelectedOutcome(firstAllow)), nil
		}
		if !wantAllow && firstReject != "" {
			return ptrResult(acp.SelectedOutcome(firstReject)), nil
		}
		return ptrResult(acp.CancelledOutcome()), nil
	}
}

func ptrResult(r acp.RequestPermissionResult) *acp.RequestPermissionResult { return &r }

// LocalFS implements fs/read_text_file and fs/write_text_file against the real
// filesystem. ACP paths MUST be absolute; Root optionally confines all access
// to one directory tree. Writes are opt-in.
type LocalFS struct {
	AllowWrite bool
	Root       string
}

func (f *LocalFS) validate(path string) *acp.RPCError {
	if path == "" || !filepath.IsAbs(path) {
		return acp.InvalidParams("path must be absolute: %q", path)
	}
	clean := filepath.Clean(path)
	if f.Root != "" {
		root := filepath.Clean(f.Root)
		rel, err := filepath.Rel(root, clean)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return acp.InvalidParams("path %q is outside root %q", path, root)
		}
	}
	return nil
}

func (f *LocalFS) ReadTextFile(_ context.Context, p acp.ReadTextFileParams) (*acp.ReadTextFileResult, *acp.RPCError) {
	if err := f.validate(p.Path); err != nil {
		return nil, err
	}
	data, e := os.ReadFile(p.Path)
	if e != nil {
		return nil, acp.Errorf(acp.CodeOther, "read_text_file: %v", e)
	}
	content := string(data)
	if p.Line != nil || p.Limit != nil {
		lines := strings.Split(content, "\n")
		start := 0
		if p.Line != nil && *p.Line > 1 {
			start = *p.Line - 1
		}
		if start > len(lines) {
			start = len(lines)
		}
		end := len(lines)
		if p.Limit != nil && *p.Limit >= 0 && start+*p.Limit < end {
			end = start + *p.Limit
		}
		content = strings.Join(lines[start:end], "\n")
	}
	return &acp.ReadTextFileResult{Content: content}, nil
}

func (f *LocalFS) WriteTextFile(_ context.Context, p acp.WriteTextFileParams) (*acp.EmptyResult, *acp.RPCError) {
	if !f.AllowWrite {
		return nil, acp.Errorf(acp.CodeOther, "fs/write_text_file is disabled by the client")
	}
	if err := f.validate(p.Path); err != nil {
		return nil, err
	}
	if e := os.WriteFile(p.Path, []byte(p.Content), 0o644); e != nil {
		return nil, acp.Errorf(acp.CodeOther, "write_text_file: %v", e)
	}
	return &acp.EmptyResult{}, nil
}

// limitedWriter keeps at most limit bytes and remembers whether it dropped any.
type limitedWriter struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	room := w.limit - w.buf.Len()
	if room < len(p) {
		if room > 0 {
			_, _ = w.buf.Write(p[:room])
		}
		w.truncated = true
		return len(p), nil // drain everything so the child never blocks
	}
	_, _ = w.buf.Write(p)
	return len(p), nil
}

func (w *limitedWriter) snapshot() (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String(), w.truncated
}

type localTerm struct {
	cancel   context.CancelFunc
	cmd      *exec.Cmd
	out      *limitedWriter
	done     chan struct{}
	mu       sync.Mutex
	exitCode *int
	signal   string
}

// LocalTerminal implements the terminal/* client capability with real
// subprocesses. Commands are launched without a shell, with args kept separate.
type LocalTerminal struct {
	mu        sync.Mutex
	terminals map[string]*localTerm
	next      atomic.Int64
}

func NewLocalTerminal() *LocalTerminal {
	return &LocalTerminal{terminals: make(map[string]*localTerm)}
}

func newTerminalID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "term_" + hex.EncodeToString(b[:])
}

func (t *LocalTerminal) Create(_ context.Context, p acp.CreateTerminalParams) (*acp.CreateTerminalResult, *acp.RPCError) {
	if p.Command == "" {
		return nil, acp.InvalidParams("terminal/create requires a command")
	}
	if p.CWD != "" && !filepath.IsAbs(p.CWD) {
		return nil, acp.InvalidParams("terminal cwd must be absolute: %q", p.CWD)
	}
	limit := 1024 * 1024
	if p.OutputByteLimit != nil && *p.OutputByteLimit > 0 {
		limit = *p.OutputByteLimit
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, p.Command, p.Args...)
	if p.CWD != "" {
		cmd.Dir = p.CWD
	}
	if len(p.Env) > 0 {
		env := append([]string{}, os.Environ()...)
		for _, e := range p.Env {
			env = append(env, e.Name+"="+e.Value)
		}
		cmd.Env = env
	}
	w := &limitedWriter{limit: limit}
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, acp.Errorf(acp.CodeInternalError, "terminal/create: %v", err)
	}
	term := &localTerm{cancel: cancel, cmd: cmd, out: w, done: make(chan struct{})}
	id := newTerminalID()
	t.mu.Lock()
	t.terminals[id] = term
	t.mu.Unlock()

	go func() {
		err := cmd.Wait()
		code := 0
		sig := ""
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
				if code == -1 {
					sig = "killed"
				}
			} else {
				code, sig = -1, err.Error()
			}
		}
		term.mu.Lock()
		term.exitCode, term.signal = &code, sig
		term.mu.Unlock()
		close(term.done)
	}()
	return &acp.CreateTerminalResult{TerminalID: id}, nil
}

func (t *LocalTerminal) get(id string) (*localTerm, *acp.RPCError) {
	t.mu.Lock()
	term := t.terminals[id]
	t.mu.Unlock()
	if term == nil {
		return nil, acp.Errorf(acp.CodeResourceNotFound, "unknown terminal %q", id)
	}
	return term, nil
}

func (t *LocalTerminal) Output(_ context.Context, p acp.TerminalOutputParams) (*acp.TerminalOutputResult, *acp.RPCError) {
	term, err := t.get(p.TerminalID)
	if err != nil {
		return nil, err
	}
	output, truncated := term.out.snapshot()
	res := &acp.TerminalOutputResult{Output: output, Truncated: truncated}
	select {
	case <-term.done:
		term.mu.Lock()
		code, sig := term.exitCode, term.signal
		term.mu.Unlock()
		status := &acp.TerminalExitStatus{Signal: sig}
		if code != nil {
			status.ExitCode = code
		}
		res.ExitStatus = status
	default:
	}
	return res, nil
}

func (t *LocalTerminal) WaitForExit(ctx context.Context, p acp.WaitForTerminalExitParams) (*acp.WaitForTerminalExitResult, *acp.RPCError) {
	term, err := t.get(p.TerminalID)
	if err != nil {
		return nil, err
	}
	select {
	case <-term.done:
	case <-ctx.Done():
		return nil, acp.Errorf(acp.CodeRequestCancelled, "wait_for_exit cancelled")
	}
	term.mu.Lock()
	defer term.mu.Unlock()
	return &acp.WaitForTerminalExitResult{ExitCode: term.exitCode, Signal: term.signal}, nil
}

func (t *LocalTerminal) Kill(_ context.Context, p acp.KillTerminalParams) (*acp.EmptyResult, *acp.RPCError) {
	term, err := t.get(p.TerminalID)
	if err != nil {
		return nil, err
	}
	term.cancel()
	return &acp.EmptyResult{}, nil
}

func (t *LocalTerminal) Release(_ context.Context, p acp.ReleaseTerminalParams) (*acp.EmptyResult, *acp.RPCError) {
	t.mu.Lock()
	term := t.terminals[p.TerminalID]
	delete(t.terminals, p.TerminalID)
	t.mu.Unlock()
	if term == nil {
		return nil, acp.Errorf(acp.CodeResourceNotFound, "unknown terminal %q", p.TerminalID)
	}
	term.cancel()
	return &acp.EmptyResult{}, nil
}

// ReleaseAll kills every remaining terminal. Call it on client shutdown.
func (t *LocalTerminal) ReleaseAll() {
	t.mu.Lock()
	terms := make([]*localTerm, 0, len(t.terminals))
	for id, term := range t.terminals {
		terms = append(terms, term)
		delete(t.terminals, id)
	}
	t.mu.Unlock()
	for _, term := range terms {
		term.cancel()
	}
}

// DescribePermission renders a permission request for CLI logs.
func DescribePermission(p *acp.RequestPermissionParams) string {
	if p == nil {
		return "<empty permission request>"
	}
	name := p.ToolCall.ToolCallID
	if p.ToolCall.Title != nil {
		name = *p.ToolCall.Title
	}
	return fmt.Sprintf("permission request: %s (session=%s, options=%d)", name, p.SessionID, len(p.Options))
}
