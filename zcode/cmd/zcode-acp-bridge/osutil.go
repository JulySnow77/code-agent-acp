package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// newFlagSet 构造统一风格的子命令 flag 集。
func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// jsonMarshalIndent 是 encoding/json 的便捷封装。
func jsonMarshalIndent(v any) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}

// runCommand 跑一次子进程并返回合并输出的去空白文本。
// stderr 一并收集: zcode 的部分诊断只走 stderr。
func runCommand(ctx context.Context, bin string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	out := strings.TrimSpace(buf.String())
	if err != nil {
		if out != "" {
			return out, execErr{err: err, out: out}
		}
		return "", err
	}
	return out, nil
}

type execErr struct {
	err error
	out string
}

func (e execErr) Error() string {
	if e.out == "" {
		return e.err.Error()
	}
	return e.err.Error() + ": " + e.out
}

func (e execErr) Unwrap() error { return e.err }

// credsPathOverride 允许测试与 doctor 指向另一个凭证文件。
func credsPathOverride() string {
	for _, k := range []string{"ZCODE_ACP_CREDS_FILE", "ZCODE_CREDS_PATH"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func envBaseURL() string {
	return strings.TrimSpace(os.Getenv("ZCODE_BASE_URL"))
}

// appVersion 读 ZCode App 的 CFBundleShortVersionString (macOS)。
// 与 CLI 版本号可能不同 —— 这正是 §2.2 里「同号构建漂移」的观察点。
func appVersion() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const app = "/Applications/ZCode.app/Contents/Info.plist"
	if _, err := os.Stat(app); err != nil {
		return ""
	}
	out, err := runCommand(ctx, "/usr/bin/defaults", nil, "read", app, "CFBundleShortVersionString")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}
