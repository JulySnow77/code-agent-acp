// Command zcode-acp-bridge 把 headless ZCode 桥接为 ACP (Agent Client Protocol) Agent。
//
// 子命令:
//
//	serve    默认。ACP JSON-RPC 2.0 over stdio。
//	doctor   启动自检: 解析 ZCODE_BIN / 推导 provider 配置 / 探测版本与协议模式 /
//	         计时 session/create / 打印模型目录 / 校验 ZCODE_ACP_MODEL 是否在目录中。
//	version  打印桥版本与构建信息。
//	record   录制协议样本到 testdata/ (ZCode 升级后执行 PORTING_PLAN §5.3 用)。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"

	"github.com/tizerluo/zcode-open-bridge-go/internal/bridge"
	"github.com/tizerluo/zcode-open-bridge-go/internal/jsonrpc"
)

// defaultBridgeVersion 是项目声明的版本号。bridgeVersion 被 ldflags 覆盖成别的
// 值时, 说明那是发布/安装构建, 以它为准 (见 buildVersion)。
const defaultBridgeVersion = "00.00.01"

// bridgeVersion 是桥自身版本。发布时用 goreleaser ldflags 覆盖; 本地构建保持
// defaultBridgeVersion, 让 version 始终能报出项目自己的版本号。
var bridgeVersion = defaultBridgeVersion

// upstreamBaseline 是移植基线 (upstream.lock 的上游 commit)。
const upstreamBaseline = "3069013"

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		args = []string{"serve"}
	}
	switch args[0] {
	case "serve":
		return cmdServe(args[1:], stdin, stdout, stderr)
	case "doctor":
		return cmdDoctor(args[1:], stdout, stderr)
	case "version":
		return cmdVersion(stdout)
	case "record":
		return cmdRecord(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	case "--version":
		// 客户端 (如 Multica daemon) 的运行时注册统一用 `<cli> --version` 探测版本。
		return cmdVersion(stdout)
	}
	fmt.Fprintf(stderr, "未知子命令 %q\n\n", args[0])
	usage(stderr)
	return 2
}

func usage(w io.Writer) {
	fmt.Fprint(w, `用法: zcode-acp-bridge <子命令> [选项]

子命令:
  serve     以 ACP agent 身份运行 (JSON-RPC 2.0 over stdio)。默认子命令。
  doctor    启动自检: ZCODE_BIN / provider 配置 / 版本 / 协议模式 /
            session/create 计时 / 模型目录 / ZCODE_ACP_MODEL 校验。
  version   打印桥版本与构建信息。
  record    录制协议样本 (ZCode 升级后按 PORTING_PLAN §5.3 执行)。
  help      打印本帮助。

环境变量见 README 与 docs/PORTING_PLAN.md §3.3。
`)
}

func cmdVersion(stdout io.Writer) int {
	fmt.Fprintf(stdout, "zcode-acp-bridge %s\n", buildVersion())
	fmt.Fprintf(stdout, "  go:          %s\n", goVersion())
	fmt.Fprintf(stdout, "  upstream:    tizerluo/zcode-open-bridge @ %s\n", upstreamBaseline)
	if info, ok := debug.ReadBuildInfo(); ok {
		goos, goarch := "", ""
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				fmt.Fprintf(stdout, "  revision:    %s%s\n", s.Value, dirtySuffix(info))
			case "GOOS":
				goos = s.Value
			case "GOARCH":
				goarch = s.Value
			}
		}
		if goos != "" || goarch != "" {
			fmt.Fprintf(stdout, "  GOOS/GOARCH: %s/%s\n", goos, goarch)
		}
	}
	return 0
}

// buildVersion 决定 version 子命令与 initialize 里报的版本号。
//
// 优先级: ldflags 注入的 bridgeVersion > 项目声明的 defaultBridgeVersion。
//
// **刻意不读 build info 的 info.Main.Version**: 在 git 检出里它总是个伪版本
// (v0.0.0-<时间>-<commit>+dirty —— 非空、也不是 "(devel)"), 于是旧实现的第一条
// 现实路径永远是「伪版本盖住声明版本」, `0.1.0-dev` 一次都露不出来, 看着像
// 项目没有版本号。伪版本里的 commit 已经在下面单独打 vcs.revision 了, 重复一遍
// 只会让人误以为那就是项目版本。
//
// 发布构建由 goreleaser 注入 {{.Version}}, 走第一条路径 (见 .goreleaser.yaml)。
func buildVersion() string {
	if bridgeVersion != defaultBridgeVersion {
		return bridgeVersion
	}
	return defaultBridgeVersion
}

func goVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.GoVersion != "" {
		return info.GoVersion
	}
	return "unknown"
}

func dirtySuffix(info *debug.BuildInfo) string {
	for _, s := range info.Settings {
		if s.Key == "vcs.modified" && s.Value == "true" {
			return " (dirty)"
		}
	}
	return ""
}

// serve 在 Phase 2 之前给出明确失败, 而不是静默地什么都不做。
func cmdServe(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	mode := fs.String("mode", "", "权限模式 yolo|build|edit|plan (覆盖 ZCODE_ACP_MODE)")
	model := fs.String("model", "", "providerId/modelId[/reasoningLevel] (覆盖 ZCODE_ACP_MODEL)")
	surface := fs.String("surface", "", "透传给 app-server 的 --surface 参数")
	legacy := fs.Bool("legacy-turn-failed", false, "恢复上游「turn.failed 当 end_turn」行为")
	verbose := fs.Bool("v", false, "详细日志")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *mode != "" {
		_ = os.Setenv("ZCODE_ACP_MODE", *mode)
	}
	if *model != "" {
		_ = os.Setenv("ZCODE_ACP_MODEL", *model)
	}
	if *surface != "" {
		_ = os.Setenv("ZCODE_ACP_SURFACE", *surface)
	}
	if *legacy {
		_ = os.Setenv("ZCODE_ACP_LEGACY_TURN_FAILED", "1")
	}
	if *verbose {
		_ = os.Setenv("ZCODE_ACP_VERBOSE", "1")
	}
	return serveLoop(stdin, stdout, stderr)
}

// serveLoop 是 serve 的主体: 启动自检 → zcode 后端 → ACP 分发层 → 读循环。
func serveLoop(stdin io.Reader, stdout, stderr io.Writer) (code int) {
	log := newLogger(stderr)
	defer func() {
		if r := recover(); r != nil {
			log("serve panic: %v", r)
			code = 1
		}
	}()

	boot := newBootstrap(log)
	if err := boot.prepare(); err != nil {
		log("启动失败: %v", err)
		fmt.Fprintln(stderr, "zcode-acp-bridge: 启动失败:", err)
		return 1
	}
	be, err := boot.start()
	if err != nil {
		log("启动后端失败: %v", err)
		fmt.Fprintln(stderr, "zcode-acp-bridge: 启动后端失败:", err)
		return 1
	}
	defer be.Close()
	log("后端就绪 (pid=%d, 协议=%s)", be.Pid(), be.Mode())

	bridge.SetVersion(buildVersion())
	conn := jsonrpc.New(stdout, log, boot.cfg.TraceNotifications)
	bridge.New(bridge.Options{
		Backend:  be,
		Version:  boot.version,
		Timeouts: boot.timeouts,
		Model:    modelFromEnv(),
		Mode:     defaultModeFromEnv(),
		Logger:   log,
	}, conn)

	// 读循环阻塞到 stdin 关闭 (客户端断开) 或 scanner 出错。
	if err := conn.Run(context.Background(), stdin); err != nil {
		log("ACP 读循环退出: %v", err)
	}
	return 0
}

// modelFromEnv 读 ZCODE_ACP_MODEL (serve 已把 --model 落到 env)。
func modelFromEnv() string {
	return strings.TrimSpace(os.Getenv("ZCODE_ACP_MODEL"))
}

// defaultModeFromEnv 解析缺省会话权限模式。ZCODE_ACP_MODE 优先 (serve 已把
// --mode 落到此); 兼容上游 ZCODE_ACP_DEFAULT_MODE; 都没设则 yolo。
func defaultModeFromEnv() string {
	for _, k := range []string{"ZCODE_ACP_MODE", "ZCODE_ACP_DEFAULT_MODE"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return "yolo"
}

// errNotImplemented 用于尚未落地的子命令, 避免静默成功。
var errNotImplemented = errors.New("尚未实现")

func notImplemented(stderr io.Writer, what string) int {
	fmt.Fprintf(stderr, "zcode-acp-bridge: %s 尚未实现\n", what)
	return 1
}

func newLogger(w io.Writer) func(string, ...any) {
	return func(format string, args ...any) {
		if !strings.HasSuffix(format, "\n") {
			format += "\n"
		}
		fmt.Fprintf(w, "[zcode-acp] "+format, args...)
	}
}
