package client

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"minimax-acp/acp"
)

// ProcessConfig describes how to launch an ACP agent subprocess. Defaults are
// `mcode acp`, the exact command Zed/JetBrains configurations use.
type ProcessConfig struct {
	Command string
	Args    []string
	Dir     string
	Env     []string // appended to the current environment
	Stderr  io.Writer
	// ClientOptions carries capabilities, handlers and trace observers.
	ClientOptions Options
}

// AgentProcess owns the child process, its stdio transport and its Client.
type AgentProcess struct {
	cmd    *exec.Cmd
	tr     acp.Transport
	client *Client

	waitOnce  sync.Once
	waitErr   error
	waitCh    chan struct{}
	closeOnce sync.Once
	closeErr  error
}

// Start launches the agent and starts the ACP connection read loop.
func Start(cfg ProcessConfig) (*AgentProcess, error) {
	command := cfg.Command
	if command == "" {
		command = "mcode"
	}
	args := cfg.Args
	if args == nil {
		args = []string{"acp"}
	}
	cmd := exec.Command(command, args...)
	if cfg.Dir != "" {
		cmd.Dir = cfg.Dir
	}
	if len(cfg.Env) > 0 {
		cmd.Env = append(os.Environ(), cfg.Env...)
	}
	stderr := cfg.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	cmd.Stderr = stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("acp: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("acp: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("acp: start %s %v: %w", command, args, err)
	}

	p := &AgentProcess{cmd: cmd, waitCh: make(chan struct{})}
	// Closing stdin makes `mcode acp` see EOF and exit on its own.
	p.tr = acp.NewLineTransport(stdout, stdin, stdin)
	p.client = Dial(p.tr, cfg.ClientOptions)

	go func() {
		p.waitErr = cmd.Wait()
		close(p.waitCh)
		p.client.Close()
	}()
	return p, nil
}

func (p *AgentProcess) Client() *Client          { return p.client }
func (p *AgentProcess) Transport() acp.Transport { return p.tr }

// Close shuts down the ACP connection, lets the child drain, and kills it if it
// does not exit within three seconds.
func (p *AgentProcess) Close() error {
	p.closeOnce.Do(func() {
		p.client.Close()
		select {
		case <-p.waitCh:
		case <-time.After(3 * time.Second):
			if p.cmd.Process != nil {
				_ = p.cmd.Process.Kill()
			}
			<-p.waitCh
		}
		if p.waitErr != nil {
			var ee *exec.ExitError
			if !errors.As(p.waitErr, &ee) || ee.ExitCode() != 0 {
				p.closeErr = fmt.Errorf("acp: agent process: %w", p.waitErr)
			}
		}
	})
	return p.closeErr
}
