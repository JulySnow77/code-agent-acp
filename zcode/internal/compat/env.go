package compat

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tizerluo/zcode-open-bridge-go/internal/zcode"
)

// Config 是桥的运行配置 (PORTING_PLAN §3.3)。
type Config struct {
	// Bin 是 zcode 可执行文件路径。推荐设为
	// /Applications/ZCode.app/Contents/Resources/glm/zcode.cjs
	Bin string
	// Surface 透传给 app-server 的 --surface 参数。空表示不传
	// (上游不传; 0.16.5 是否仍接受该 flag 未实测, 故默认关闭)。
	Surface string
	// Mode 是建会话的默认权限模式: yolo|build|edit|plan。
	Mode string
	// Model 是 ZCODE_ACP_MODEL: providerId/modelId[/reasoningLevel]。
	Model string
	// LegacyTurnFailed 恢复上游「turn.failed 当 end_turn」行为 (§8 D4)。
	LegacyTurnFailed bool
	// WireLog 是线材日志路径。
	WireLog string
	// Verbose 打开详细日志。
	Verbose bool
	// TraceNotifications 打开「被忽略的未知通知」日志 (ZCODE_ACP_TRACE_NOTIF=1)。
	// 与 Verbose 分开: telemetry 类通知一个 turn 上千条, 连 -v 下都不该打。
	TraceNotifications bool
}

// ConfigFromEnv 读取环境变量与默认值。
func ConfigFromEnv() Config {
	return Config{
		Bin:                firstNonEmpty(os.Getenv("ZCODE_BIN"), "zcode"),
		Surface:            os.Getenv("ZCODE_ACP_SURFACE"),
		Mode:               firstNonEmpty(os.Getenv("ZCODE_ACP_MODE"), "yolo"),
		Model:              os.Getenv("ZCODE_ACP_MODEL"),
		LegacyTurnFailed:   os.Getenv("ZCODE_ACP_LEGACY_TURN_FAILED") == "1",
		WireLog:            os.Getenv("ZCODE_ACP_WIRE_LOG"),
		Verbose:            os.Getenv("ZCODE_ACP_VERBOSE") == "1",
		TraceNotifications: os.Getenv("ZCODE_ACP_TRACE_NOTIF") == "1",
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// PassThroughVars 是允许透传给子进程的 ZCode 相关环境变量 (§2.2 C12)。
// 它们本就在 os.Environ() 里会自然继承, 列出仅为 doctor 报告与文档对齐。
var PassThroughVars = []string{
	"ZCODE_DATA_BASE_DIR",
	"ZCODE_BUILTIN_PROVIDER_CONFIG_FILE",
	"ZCODE_PERSONAL_PROVIDER_CONFIG_FILE",
	"ZCODE_CREDENTIAL_SECRET",
	"ZCODE_BASE_URL",
	"ZCODE_ENDPOINT_ORIGIN",
	"ZCODE_APP_VERSION",
	"ZCODE_HTTP_PROXY",
	"ZCODE_NO_PROXY",
	"ZCODE_MODEL",
	"ANTHROPIC_API_KEY",
}

// SecretVars 是打印时需要脱敏的变量。
var SecretVars = map[string]bool{
	"ZCODE_CREDENTIAL_SECRET": true,
	"ANTHROPIC_API_KEY":       true,
}

// Finding 是环境推导过程中的一条观察 (warn/error 级会让 doctor 报红)。
type Finding struct {
	Level string // "error" | "warn" | "info"
	Msg   string
}

// EnvReport 描述最终注入子进程的环境, 供 doctor 输出。
type EnvReport struct {
	Bin            string            // 用户配置值
	ResolvedBin    string            // 解析后的绝对路径 (symlink 已解)
	Args           []string          // 子命令与参数
	ProviderConfig string            // 最终使用的 provider 配置文件
	ProviderSource string            // "env" | "derived" | ""
	Injected       map[string]string // 注入项 (含凭证, 已脱敏)
	Passed         map[string]string // 透传项 (已脱敏)
	Findings       []Finding
}

// DeriveEnv 解析 ZCODE_BIN、推导/校验 provider 配置文件, 并合并凭证,
// 返回可直接交给子进程的环境。
//
// §2.2 C1: 0.16.5 按入口文件目录找 provider/zcode-builtin.json 或
// ../../../../../config/provider/zcode-builtin.json; 上游 README 的 symlink
// 方案因此失效, 启动即退出并报「无法定位 CLI ZCode Built-in Provider Config」。
func DeriveEnv(cfg Config, creds map[string]string) (EnvReport, []string, error) {
	rep := EnvReport{
		Bin:      cfg.Bin,
		Injected: map[string]string{},
		Passed:   map[string]string{},
	}

	resolved, err := ResolveBin(cfg.Bin)
	if err != nil {
		rep.Findings = append(rep.Findings, Finding{Level: "error", Msg: err.Error()})
		return rep, nil, err
	}
	rep.ResolvedBin = resolved

	args := []string{"app-server", "--stdio"}
	if cfg.Surface != "" {
		args = append(args, "--surface", cfg.Surface)
	}
	rep.Args = args

	// ---- provider 配置文件 (C1) ----
	if v := os.Getenv("ZCODE_BUILTIN_PROVIDER_CONFIG_FILE"); v != "" {
		if _, statErr := os.Stat(v); statErr == nil {
			rep.ProviderConfig, rep.ProviderSource = v, "env"
			rep.Findings = append(rep.Findings, Finding{"info",
				fmt.Sprintf("provider 配置来自环境变量: %s", v)})
		} else {
			rep.Findings = append(rep.Findings, Finding{"warn",
				fmt.Sprintf("ZCODE_BUILTIN_PROVIDER_CONFIG_FILE=%s 指向的文件不存在, 尝试自动推导", v)})
		}
	}
	if rep.ProviderConfig == "" {
		p, ok := DeriveProviderConfig(resolved)
		if ok {
			rep.ProviderConfig, rep.ProviderSource = p, "derived"
			rep.Findings = append(rep.Findings, Finding{"info",
				fmt.Sprintf("provider 配置按 ZCODE_BIN 推导: %s", p)})
		} else {
			rep.Findings = append(rep.Findings, Finding{"error",
				fmt.Sprintf("未能定位 provider 配置文件: 已从 %s 向上查找 %d 级仍未找到 "+
					"config/provider/zcode-builtin.json；请显式设置 ZCODE_BUILTIN_PROVIDER_CONFIG_FILE",
					resolved, providerSearchDepth)})
		}
	}

	// ---- 组装环境 ----
	env := os.Environ()
	env = setEnv(env, "ZCODE_BUILTIN_PROVIDER_CONFIG_FILE", rep.ProviderConfig)

	// 个人注册表必须与内置表**成对**提供。上游
	// provider-node/src/runtime-paths.ts:resolveNodeProviderRuntimePaths 遇到
	// 只给一个的组合会直接抛 "ZCode Built-in 与 Personal Provider Config
	// 路径必须同时提供"; 桌面端 spawn Agent 时也是成对注入的
	// (services/src/node.ts: createNodeProviderRuntimePathEnv)。
	// 之前只注入内置表, 个人路径靠环境里恰好有值, 属于碰运气。
	if v := strings.TrimSpace(os.Getenv(personalProviderConfigEnv)); v != "" {
		env = setEnv(env, personalProviderConfigEnv, v)
		rep.Passed[personalProviderConfigEnv] = v
	} else if p, err := defaultPersonalProviderConfigPath(); err == nil {
		if _, statErr := os.Stat(p); statErr == nil {
			env = setEnv(env, personalProviderConfigEnv, p)
			rep.Injected[personalProviderConfigEnv] = p
		}
	}

	// 凭证: 显式非空环境变量优先, 空串视为未设置 (保留 config 值)。
	for _, k := range []string{"ZCODE_MODEL", "ZCODE_BASE_URL", "ANTHROPIC_API_KEY"} {
		if v, ok := creds[k]; ok && v != "" {
			if strings.TrimSpace(os.Getenv(k)) == "" {
				env = setEnv(env, k, v)
				rep.Injected[k] = redact(k, v)
			}
		}
	}
	if v := os.Getenv("ZCODE_BASE_URL"); v != "" {
		rep.Passed["ZCODE_BASE_URL"] = redact("ZCODE_BASE_URL", v)
	}
	if v := os.Getenv("ZCODE_MODEL"); v != "" {
		rep.Passed["ZCODE_MODEL"] = v
	}
	if v := rep.ProviderConfig; v != "" {
		rep.Injected["ZCODE_BUILTIN_PROVIDER_CONFIG_FILE"] = v
	}
	for _, k := range PassThroughVars {
		if v := os.Getenv(k); v != "" {
			rep.Passed[k] = redact(k, v)
		}
	}
	return rep, env, nil
}

// providerSearchDepth 是从 ZCODE_BIN 所在目录向上查找的层数。
const providerSearchDepth = 6

// ResolveBin 把用户给的 ZCODE_BIN 解析成绝对路径 (解 symlink)。
// 相对/裸命令走 PATH 查找。
func ResolveBin(bin string) (string, error) {
	if strings.TrimSpace(bin) == "" {
		return "", fmt.Errorf("ZCODE_BIN 为空")
	}
	p := bin
	if !filepath.IsAbs(p) {
		found, err := exec.LookPath(p)
		if err != nil {
			return "", fmt.Errorf("ZCODE_BIN=%q 无法解析: %w", bin, err)
		}
		p = found
	}
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("ZCODE_BIN=%q 不存在: %w", bin, err)
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return filepath.Abs(p)
}

// DeriveProviderConfig 定位 CLI 要读的内置 provider 表。
//
// 优先桌面按 endpoint 隔离物化的那份
// (~/.zcode/v2/runtime/provider/<platform>-<arch>/<version>/endpoint-*/zcode-builtin.json):
// 3.14+ 的 app-server 就是用它算 basedOnZCodeBuiltinRevision 的
// (sha256 路径), 换成 App 包里的 bundled 副本会让指纹对不上, 后端
// 静默丢弃账号快照 —— 表现是「明明有订阅, 目录里一个 account:* 模型都
// 没有」。找不到再退回按 ZCODE_BIN 相对位置查找 (上游 §2.2 C1)。
func DeriveProviderConfig(resolvedBin string) (string, bool) {
	if p := zcode.BuiltinProviderPath(); p != "" {
		return p, true
	}
	dir := filepath.Dir(resolvedBin)
	for i := 0; i < providerSearchDepth; i++ {
		candidate := filepath.Join(dir, "config", "provider", "zcode-builtin.json")
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			return candidate, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", false
}

// setEnv 覆盖或追加一个环境变量。
func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

// redact 对敏感值做最小脱敏 (保留前 4 位与长度信息, 便于排查又不泄漏)。
func redact(key, value string) string {
	if !SecretVars[key] || value == "" {
		return value
	}
	if len(value) <= 8 {
		return "***"
	}
	return value[:4] + "***(" + fmt.Sprint(len(value)) + ")"
}

// SortedKeys 返回 map 的有序键, 用于 doctor 的稳定输出。
func SortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// personalProviderConfigEnv 与上游 provider-node/src/runtime-paths.ts 同名。
const personalProviderConfigEnv = "ZCODE_PERSONAL_PROVIDER_CONFIG_FILE"

// defaultPersonalProviderConfigPath 推导 ~/.zcode/v2/provider_config.json。
// 上游用 getAppConfigDir() (services/src/model-provider/providerConfigRuntime.ts:
// PERSONAL_PROVIDER_CONFIG_FILE_NAME); CLI 与桌面同址。ZCODE_HOME 在这里
// 同样生效, 与 zcodeV2Dir 的优先级一致。
func defaultPersonalProviderConfigPath() (string, error) {
	base := strings.TrimSpace(os.Getenv("ZCODE_HOME"))
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".zcode")
	}
	return filepath.Join(base, "v2", "provider_config.json"), nil
}
