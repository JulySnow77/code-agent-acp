package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tizerluo/zcode-open-bridge-go/internal/compat"
	"github.com/tizerluo/zcode-open-bridge-go/internal/creds"
	"github.com/tizerluo/zcode-open-bridge-go/internal/wirelog"
	"github.com/tizerluo/zcode-open-bridge-go/internal/zcode"
)

// bootstrap 是启动自检链 (PORTING_PLAN §3.2 规则 3):
//
//  1. 解析 ZCODE_BIN 真实路径
//  2. 推导/校验 provider 配置文件
//  3. 合并凭证
//  4. 启动子进程并探测 zcode --version 与协议模式
//
// 任一步失败立即以可读错误退出 —— 不要等客户端 session/new 才发现。
type bootstrap struct {
	cfg      compat.Config
	report   compat.EnvReport
	env      []string
	creds    map[string]string
	timeouts compat.Timeouts
	version  zcode.Version
	wire     *wirelog.Recorder
	logger   func(string, ...any)
}

func newBootstrap(log func(string, ...any)) *bootstrap {
	return &bootstrap{
		cfg:      compat.ConfigFromEnv(),
		timeouts: compat.TimeoutsFromEnv(),
		logger:   log,
	}
}

// prepare 做第 1~3 步 (不启动子进程)。
func (b *bootstrap) prepare() error {
	path := ""
	if v := credsPathOverride(); v != "" {
		path = v
	}
	b.creds = creds.Load(path, func(msg string) { b.logger("⚠ %s", msg) })

	// baseURL 残留自愈 (creds 包的第二条规定)。
	if envBU, configBU := envBaseURL(), b.creds["ZCODE_BASE_URL"]; envBU != "" && configBU != "" {
		if creds.IsStaleEnvBaseURL(envBU, configBU, creds.AllProviderBaseURLs(path)) {
			b.creds["ZCODE_BASE_URL"] = configBU
			b.logger("⚠ ZCODE_BASE_URL 残留检测: env='%s' 是 config 里另一个 provider 的 "+
				"endpoint, 已自动用 config 值 '%s' (旧 plan 残留)", envBU, configBU)
		}
	}

	report, env, err := compat.DeriveEnv(b.cfg, b.creds)
	b.report, b.env = report, env
	if err != nil {
		return err
	}
	// provider 配置缺失是可恢复的警告还是致命错误, 由调用方决定:
	// doctor 报红但继续 (让用户看到其它项的诊断), serve 直接退出。
	if report.ProviderConfig == "" {
		return fmt.Errorf("未能定位 provider 配置文件 (ZCODE_BUILTIN_PROVIDER_CONFIG_FILE)")
	}
	b.wire = wirelog.New(b.cfg.WireLog)
	return nil
}

// start 启动后端子进程 (第 4 步)。
func (b *bootstrap) start() (*zcode.Backend, error) {
	if b.version.Zero() {
		if v, err := detectZcodeVersion(b.report.ResolvedBin, b.env); err != nil {
			b.logger("⚠ zcode --version 探测失败: %v", err)
		} else {
			b.version = v
		}
	}
	be, err := zcode.Start(zcode.Options{
		Bin:                b.report.ResolvedBin,
		Args:               b.report.Args,
		Env:                b.env,
		Logger:             b.logger,
		TraceNotifications: b.cfg.TraceNotifications,
		Wire:               b.wire,
		ProbeTimeout:       probeTimeout(),
	})
	if err != nil {
		return nil, err
	}
	return be, nil
}

// backend 是一次性完成 prepare + start 的便捷入口 (serve 用)。
func startBackend(log func(string, ...any)) (*zcode.Backend, error) {
	b := newBootstrap(log)
	if err := b.prepare(); err != nil {
		return nil, err
	}
	return b.start()
}

// versionProbeTimeout 覆盖 `zcode --version` 的冷启动开销。
// 实测本机 (1.2GB 会话库) 需要约 21s, 远大于直觉值; 上游从未探测过版本,
// 这是 Go 版新增的启动自检项, 超时给足以免误报失败。
const versionProbeTimeout = 60 * time.Second

// probeTimeout 是协议探测窗口, 可用 ZCODE_ACP_PROBE_TIMEOUT (秒) 覆盖。
func probeTimeout() time.Duration {
	if s := strings.TrimSpace(os.Getenv("ZCODE_ACP_PROBE_TIMEOUT")); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 0 // 用 zcode 包默认
}

// detectZcodeVersion 跑 `zcode --version` 拿版本号。
func detectZcodeVersion(bin string, env []string) (zcode.Version, error) {
	ctx, cancel := context.WithTimeout(context.Background(), versionProbeTimeout)
	defer cancel()
	out, err := runCommand(ctx, bin, env, "--version")
	if err != nil {
		return zcode.Version{}, err
	}
	return zcode.ParseVersion(out)
}
