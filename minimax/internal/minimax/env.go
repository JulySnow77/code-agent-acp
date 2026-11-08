// Package minimax holds read-only detection of the MiniMax Code CLI and the
// host environment that can affect `mcode acp`.
//
// It deliberately never opens a credential file: it only stats directory
// names under the data dir, runs `mcode --version` / `mcode acp --help`, and
// inspects environment variables. Everything here is safe to run in a probe
// report that will be committed to a repository.
package minimax

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// DefaultCommand is the CLI installed by `npm install -g @minimax-ai/code`.
	DefaultCommand = "mcode"
	// DefaultArgs is the native ACP v1 stdio server subcommand.
	DefaultArgs = "acp"
	// CLIOverrideEnv is OUR OWN override (not a MiniMax environment variable);
	// it lets an ACP host pin a specific CLI path without changing flags.
	CLIOverrideEnv = "MINIMAX_ACP_CLI"
	// DataDirEnv names are the four overrides a desktop host is known to strip
	// before launching a native MiniMax ACP process. Their presence means the
	// CLI may read a different account than the one we are inspecting.
	DataDirEnv = "MINIMAX_DATA_DIR"
)

var dataDirOverrides = []string{
	DataDirEnv,
	"MAVIS_DATA_DIR",
	"__MAVIS_RUNTIME_DATA_DIR",
	"__MAVIS_RUNTIME_PROFILE",
}

var proxyEnvNames = []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "https_proxy", "http_proxy", "all_proxy"}

// CommandFromEnv resolves the CLI binary: --command wins at the call site,
// otherwise MINIMAX_ACP_CLI, otherwise `mcode`.
func CommandFromEnv() string {
	if v := strings.TrimSpace(os.Getenv(CLIOverrideEnv)); v != "" {
		return v
	}
	return DefaultCommand
}

// Runner executes a command with a context and returns combined stdout.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Inspector performs read-only environment detection. All collaborators are
// injectable so tests never spawn a real CLI or read the user's home dir.
type Inspector struct {
	LookPath  func(string) (string, error)
	Run       Runner
	LookupEnv func(string) (string, bool)
	Stat      func(string) (os.FileInfo, error)
	Home      string
	Timeout   time.Duration
}

// Environment is the JSON-safe result of Inspect. It contains no credential
// material, no session ids and no file contents.
type Environment struct {
	Command       string   `json:"command"`
	ResolvedPath  string   `json:"resolvedPath,omitempty"`
	CLIVersion    string   `json:"cliVersion,omitempty"`
	ACPServerHelp bool     `json:"acpServerHelp"`
	DataDir       string   `json:"dataDir,omitempty"`
	DataDirExists bool     `json:"dataDirExists"`
	AuthDirExists bool     `json:"authDirExists"`
	Region        string   `json:"region,omitempty"`
	LoginHint     string   `json:"loginHint"`
	Warnings      []string `json:"warnings,omitempty"`
	Error         string   `json:"error,omitempty"`
}

// New builds an Inspector backed by the real OS.
func New() *Inspector {
	return &Inspector{
		LookPath:  exec.LookPath,
		Run:       realRunner,
		LookupEnv: os.LookupEnv,
		Stat:      os.Stat,
		Home:      homeDir(),
		Timeout:   10 * time.Second,
	}
}

func homeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}

func realRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// LoginHint returns the correct sign-in command for a region. An empty region
// means a mainland China account (the CLI default).
func LoginHint(command, region string) string {
	if command == "" {
		command = DefaultCommand
	}
	region = strings.TrimSpace(region)
	if region == "" {
		return fmt.Sprintf("`%s login`（Global 账号用 `%s login --region global`）", command, command)
	}
	return fmt.Sprintf("`%s login --region %s` 或 `%s acp login --region %s`", command, region, command, region)
}

// Inspect gathers version, native ACP command availability, data-dir presence
// and host-environment warnings. It never sends an ACP frame and never opens
// a credential file.
func (i *Inspector) Inspect(command, region string) Environment {
	if command == "" {
		command = CommandFromEnv()
	}
	timeout := i.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	out := Environment{
		Command:   command,
		Region:    strings.TrimSpace(region),
		LoginHint: LoginHint(command, region),
	}

	if i.LookPath != nil {
		if resolved, err := i.LookPath(command); err == nil {
			out.ResolvedPath = resolved
		} else {
			out.Error = fmt.Sprintf("%s not found on PATH: %v", command, err)
			out.Warnings = append(out.Warnings,
				"CLI 不在 PATH 上；确认已安装 @minimax-ai/code，或用 --command 指定全路径")
		}
	}

	if out.Error == "" && i.Run != nil {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		ver, err := i.Run(ctx, command, "--version")
		cancel()
		if err != nil {
			out.Warnings = append(out.Warnings, fmt.Sprintf("无法读取 `%s --version`：%v", command, err))
		} else {
			out.CLIVersion = strings.TrimSpace(string(ver))
		}

		ctx2, cancel2 := context.WithTimeout(context.Background(), timeout)
		help, err := i.Run(ctx2, command, DefaultArgs, "--help")
		cancel2()
		if err != nil {
			out.Warnings = append(out.Warnings, fmt.Sprintf("`%s acp --help` 未成功：%v", command, err))
		} else if strings.Contains(strings.ToLower(string(help)), "agent client protocol") {
			out.ACPServerHelp = true
		} else {
			out.Warnings = append(out.Warnings, "`acp --help` 输出中未找到 ACP server 说明，CLI 版本可能不支持原生 ACP")
		}
	}

	// Directory names only. Never read auth.json, config.yaml or sessions/*.
	if i.Home != "" && i.Stat != nil {
		dataDir := filepath.Join(i.Home, ".minimax")
		out.DataDir = dataDir
		if _, err := i.Stat(dataDir); err == nil {
			out.DataDirExists = true
			if _, err := i.Stat(filepath.Join(dataDir, "auth")); err == nil {
				out.AuthDirExists = true
			} else {
				out.Warnings = append(out.Warnings, "数据目录存在但没有 auth/ 目录，可能尚未登录")
			}
		} else {
			out.Warnings = append(out.Warnings,
				"未发现 ~/.minimax 数据目录；ACP 会话使用 CLI 自身的登录态")
		}
	}

	if i.LookupEnv != nil {
		for _, key := range dataDirOverrides {
			if v, ok := i.LookupEnv(key); ok && v != "" {
				out.Warnings = append(out.Warnings, fmt.Sprintf(
					"%s 已设置：ACP runtime 可能读取与当前登录态不同的数据目录", key))
			}
		}
		if v, ok := i.LookupEnv("NODE_OPTIONS"); ok && strings.Contains(v, "--require") {
			out.Warnings = append(out.Warnings,
				"NODE_OPTIONS 注入了 --require shim；在本机实测中 `mcode acp` 因此无任何 stdio 响应")
		}
		if v, ok := i.LookupEnv("PATH"); ok &&
			(strings.Contains(v, "brokered-bin") || strings.Contains(v, "safe-bin")) {
			out.Warnings = append(out.Warnings,
				"PATH 含 safe-delete / brokered shim；CLI 清理锁文件时可能被拦截并退出")
		}
		for _, key := range proxyEnvNames {
			if v, ok := i.LookupEnv(key); ok && v != "" {
				out.Warnings = append(out.Warnings,
					fmt.Sprintf("%s 已设置（CLI 会读取该代理；localhost 仍绕过）", key))
				break
			}
		}
	}
	return out
}
