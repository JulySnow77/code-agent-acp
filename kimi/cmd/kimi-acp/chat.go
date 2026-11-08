package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"kimi-acp/acp"
	"kimi-acp/client"
)

// chat drives a real `kimi acp` session over stdio. Agent stdout is consumed by
// the ACP client; this command's stdout is for the human user only.
func runChat(args []string) int {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	var o commonOpts
	addCommonFlags(fs, &o)
	prompt := fs.String("prompt", "", "one-shot prompt text (empty = interactive REPL)")
	fs.StringVar(prompt, "p", "", "shorthand for --prompt")
	sessionID := fs.String("session", "", "existing sessionId to load/resume")
	resume := fs.Bool("resume", false, "prefer session/resume over session/load")
	permissionMode := fs.String("permission-mode", "deny", "permission policy: deny | allow | prompt")
	allowWrite := fs.Bool("allow-write", false, "advertise and serve fs/write_text_file")
	allowTerminal := fs.Bool("allow-terminal", false, "advertise and serve terminal/*")
	raw := fs.Bool("raw", false, "print raw session/update JSON instead of rendered text")
	imagePath := fs.String("image", "", "absolute path of an image to attach to a one-shot prompt")
	stdinOnce := fs.Bool("stdin", false, "read one prompt from stdin instead of a REPL")
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	mode := client.PermissionMode(*permissionMode)
	if mode != client.PermissionDeny && mode != client.PermissionAllow && *permissionMode != "prompt" {
		fmt.Fprintf(os.Stderr, "chat: --permission-mode must be deny, allow or prompt\n")
		return 2
	}

	trace, err := openTrace(o.trace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chat: cannot open trace: %v\n", err)
		return 1
	}
	defer trace.Close()

	localFS := &client.LocalFS{AllowWrite: *allowWrite, Root: ""}
	var localTerm *client.LocalTerminal
	if *allowTerminal {
		localTerm = client.NewLocalTerminal()
		defer localTerm.ReleaseAll()
	}

	var outMu sync.Mutex
	write := func(format string, a ...any) {
		outMu.Lock()
		defer outMu.Unlock()
		fmt.Fprintf(os.Stdout, format, a...)
	}
	reader := bufio.NewReader(os.Stdin)

	handlers := client.HandlerFuncs{
		SessionUpdate: func(n *acp.SessionNotification) {
			if n == nil {
				return
			}
			if *raw {
				b, _ := json.Marshal(n)
				write("%s\n", string(b))
				return
			}
			u := n.Update
			switch u.Type() {
			case acp.UpdateAgentMessageChunk:
				write("%s", u.Text())
			case acp.UpdateUserMessageChunk:
				// The user's own text is already on screen.
			case acp.UpdateAgentThoughtChunk:
				write("[thought] %s\n", u.Text())
			case acp.UpdateToolCall:
				write("\n[tool] %s (kind=%s status=%s)\n", u.ToolTitle(), u.ToolKind(), u.ToolStatus())
			case acp.UpdateToolCallUpdate:
				write("[tool %s] %s\n", u.ToolCallID(), u.ToolStatus())
			case acp.UpdatePlan:
				write("\n[plan]\n")
			case acp.UpdateNotice:
				write("\n[notice] %s: %s\n", u.Str("severity"), u.Title())
			case acp.UpdateUsage:
				write("\n[usage] %v/%v\n", u["used"], u["size"])
			default:
				write("\n[update] %s\n", u.Type())
			}
		},
		ReadTextFile:  localFS.ReadTextFile,
		WriteTextFile: nil,
	}
	if *allowWrite {
		handlers.WriteTextFile = localFS.WriteTextFile
	}
	if *permissionMode == "prompt" {
		handlers.RequestPermission = func(_ context.Context, p *acp.RequestPermissionParams) (*acp.RequestPermissionResult, *acp.RPCError) {
			write("\n[permission] %s\n", client.DescribePermission(p))
			for i, opt := range p.Options {
				write("  %d) %s [%s]\n", i+1, opt.Name, opt.Kind)
			}
			write("choose number (0 = cancel): ")
			line, _ := reader.ReadString('\n')
			line = strings.TrimSpace(line)
			if line == "" || line == "0" {
				return ptr(acp.CancelledOutcome()), nil
			}
			var idx int
			if _, err := fmt.Sscanf(line, "%d", &idx); err != nil || idx < 1 || idx > len(p.Options) {
				return ptr(acp.CancelledOutcome()), nil
			}
			return ptr(acp.SelectedOutcome(p.Options[idx-1].OptionID)), nil
		}
	} else {
		auto := client.AutoPermission(mode)
		handlers.RequestPermission = func(ctx context.Context, p *acp.RequestPermissionParams) (*acp.RequestPermissionResult, *acp.RPCError) {
			fmt.Fprintf(os.Stderr, "[permission] %s -> policy=%s\n", client.DescribePermission(p), mode)
			return auto(ctx, p)
		}
	}
	if localTerm != nil {
		handlers.TerminalCreate = localTerm.Create
		handlers.TerminalOutput = localTerm.Output
		handlers.TerminalWaitExit = localTerm.WaitForExit
		handlers.TerminalKill = localTerm.Kill
		handlers.TerminalRelease = localTerm.Release
	}

	copts := client.Options{
		Info: acp.Implementation{Name: "kimi-acp-go-chat", Title: "kimi-acp Go chat", Version: version},
		Capabilities: acp.ClientCapabilities{
			FS:       &acp.FileSystemCapabilities{ReadTextFile: true, WriteTextFile: *allowWrite},
			Terminal: *allowTerminal,
		},
		Handlers: handlers,
		ConnOptions: acp.ConnOptions{
			Logger:   newLogger(),
			Observer: trace.observer("chat"),
		},
	}

	proc, err := startProcess(o, copts, os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chat: %v\n", err)
		return 1
	}
	defer proc.Close()

	appCtx, stop := appContext()
	defer stop()
	ctx, cancel := context.WithTimeout(appCtx, o.timeout)
	defer cancel()

	initResult, err := proc.Client().Initialize(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chat: initialize: %v\n", err)
		return 1
	}
	agentName, agentVersion := "unknown-agent", "?"
	if initResult.AgentInfo != nil {
		if initResult.AgentInfo.Name != "" {
			agentName = initResult.AgentInfo.Name
		}
		if initResult.AgentInfo.Version != "" {
			agentVersion = initResult.AgentInfo.Version
		}
	}
	fmt.Fprintf(os.Stderr, "connected: %s v%s (ACP v%d)\n", agentName, agentVersion, initResult.ProtocolVersion)

	activeSession := *sessionID
	if activeSession != "" {
		if *resume && initResult.AgentCapabilities.CanResume() {
			_, err = proc.Client().ResumeSession(ctx, acp.ResumeSessionParams{SessionID: activeSession, CWD: o.cwd})
		} else if initResult.AgentCapabilities.CanLoadSession() {
			_, err = proc.Client().LoadSession(ctx, acp.LoadSessionParams{SessionID: activeSession, CWD: o.cwd})
		} else {
			fmt.Fprintf(os.Stderr, "chat: agent cannot load sessions\n")
			return 1
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "chat: load session: %v\n", err)
			return 1
		}
	} else {
		ns, err := proc.Client().NewSession(ctx, acp.NewSessionParams{CWD: o.cwd})
		if err != nil {
			var rpcErr *acp.RPCError
			if errors.As(err, &rpcErr) && rpcErr.Code == acp.CodeAuthRequired {
				fmt.Fprintf(os.Stderr, "chat: Kimi CLI 需要登录：先在终端运行 `kimi` 完成 /login，或使用 `kimi acp --login` 的终端认证流程。\n")
				return 3
			}
			fmt.Fprintf(os.Stderr, "chat: session/new: %v\n", err)
			return 1
		}
		activeSession = ns.SessionID
		fmt.Fprintf(os.Stderr, "session: %s\n", activeSession)
	}

	buildBlocks := func(text string) ([]acp.ContentBlock, error) {
		blocks := []acp.ContentBlock{acp.TextBlock(text)}
		if *imagePath != "" {
			data, err := os.ReadFile(*imagePath)
			if err != nil {
				return nil, err
			}
			mime := "image/png"
			switch strings.ToLower(filepath.Ext(*imagePath)) {
			case ".jpg", ".jpeg":
				mime = "image/jpeg"
			case ".webp":
				mime = "image/webp"
			case ".gif":
				mime = "image/gif"
			}
			blocks = append(blocks, acp.ImageBlock(base64.StdEncoding.EncodeToString(data), mime))
		}
		return blocks, nil
	}

	sendPrompt := func(text string) int {
		blocks, err := buildBlocks(text)
		if err != nil {
			fmt.Fprintf(os.Stderr, "chat: image: %v\n", err)
			return 1
		}
		result, err := proc.Client().Prompt(ctx, activeSession, blocks)
		if err != nil {
			var rpcErr *acp.RPCError
			if errors.As(err, &rpcErr) && rpcErr.Code == acp.CodeAuthRequired {
				fmt.Fprintf(os.Stderr, "chat: Kimi CLI 需要登录：先运行 `kimi` 完成 /login。\n")
				return 3
			}
			fmt.Fprintf(os.Stderr, "\nchat: prompt: %v\n", err)
			return 1
		}
		write("\n[stop] %s\n", result.StopReason)
		return 0
	}

	if *prompt != "" {
		return sendPrompt(*prompt)
	}
	if *stdinOnce {
		line, _ := reader.ReadString('\n')
		if strings.TrimSpace(line) == "" {
			return 0
		}
		return sendPrompt(strings.TrimRight(line, "\r\n"))
	}

	for {
		write("you> ")
		line, err := reader.ReadString('\n')
		if err != nil {
			write("\n")
			return 0
		}
		text := strings.TrimSpace(line)
		if text == "" {
			continue
		}
		if text == "/exit" || text == "/quit" {
			return 0
		}
		if text == "/cancel" {
			if err := proc.Client().Cancel(ctx, activeSession); err != nil {
				fmt.Fprintf(os.Stderr, "cancel: %v\n", err)
			}
			continue
		}
		if code := sendPrompt(text); code != 0 {
			return code
		}
	}
}

func ptr[T any](v T) *T { return &v }
