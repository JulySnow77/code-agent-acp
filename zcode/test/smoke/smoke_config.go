//go:build real

package smoke

import (
	"strconv"
)

// 真机用例的**测试侧**配置 —— 只影响这个测试进程自己怎么跑, 桥和 zcode 都
// 不读它们。
//
// 为什么这些不再走环境变量: env 文件是给「要跨进程边界的参数」准备的
// (见 env.go)。测试自己用的东西 (在哪个目录干活、答不答、选哪个选项) 压根不
// 需要出进程, 塞进 os.Environ() 有两个实际代价:
//   - 它们会随 os.Environ() 继承给桥和 zcode 子进程, 而那两个进程根本不读
//     ZCODE_SMOKE_*, 只是让环境变脏, 排查时更难看出「谁真正消费了这个值」;
//   - os.Getenv 满天飞, 谁读了、为什么读, 要 grep 全包才看得清。
//
// 读法统一走这里, 优先级不变: 命令行 flag > env 文件 > 环境变量残留
// (只有 ZCODE_SMOKE_ENV_KEEP_EXISTING=1 时才允许环境变量说话, 与 env.go 同一
// 条规则, 免得出现「一半听文件的、一半听 shell 的」这种没法推理的混合)。
//
// 真正必须留在环境里的只有两类: 桥进程自己 os.Getenv 的那几个
// (ZCODE_BIN / ZCODE_ACP_MODEL / ZCODE_ACP_BRIDGE_BIN 之外的桥选项), 以及
// zcode 子进程要读的 ZCODE_{BUILTIN,PERSONAL}_PROVIDER_CONFIG_FILE —— 桥只能
// 通过子进程环境把后者转交给 zcode, 没有别的通道。

// smokeKeys 是所有测试侧配置键。用于报告与「文件里写了什么」的对照。
var smokeKeys = []string{
	"ZCODE_ACP_BRIDGE_BIN",
	"ZCODE_SMOKE_PROJECT_DIR",
	"ZCODE_SMOKE_PROMPT",
	"ZCODE_SMOKE_TASK_TIMEOUT_MIN",
	"ZCODE_SMOKE_ASK_PROMPT",
	"ZCODE_SMOKE_ASK_WORKSPACE",
	"ZCODE_SMOKE_ASK_FALLBACK",
	"ZCODE_SMOKE_INTERACTIVE",
	"ZCODE_ACP_SMOKE_PLAN_PROMPT",
	"ZCODE_ACP_SMOKE_PERMISSION_OPTION",
	"ZCODE_ACP_SMOKE_PERMISSION_PROMPT",
	"ZCODE_ACP_SMOKE_PERMISSION_WORKSPACE",
}

// smokeCfg 是从 env 文件解析出来的测试侧配置。
//
// 只在 TestMain 里赋值一次, 之后只读 —— 所以用普通 map 而不是每次 Getenv,
// 免得读到一半文件被改了行为就飘。
var smokeCfg = map[string]string{}

// setSmokeConfig 由 LoadSmokeEnv 填。分文件与普通环境两路, 便于报告说清每个值
// 的来源 (排查「为什么我改了 env 没生效」时, 来源比值本身更重要)。
func setSmokeConfig(fromFile, keptFromEnv map[string]string) {
	smokeCfg = make(map[string]string, len(smokeKeys))
	for k, v := range fromFile {
		smokeCfg[k] = v
	}
	for k, v := range keptFromEnv {
		smokeCfg[k] = v // keep-existing 模式下环境变量赢, 与 env.go 的覆盖方向一致
	}
}

// smokeStr 读一个测试侧配置项; 没配返回空串 (调用方通常拿它跟 "1" 比)。
func smokeStr(key string) string { return smokeCfg[key] }

// smokeFlag 读一个开关型配置 (值为 "1" 即真)。
func smokeFlag(key string) bool { return smokeCfg[key] == "1" }

// smokeInt 读一个整数配置, 配了但不是合法正整数时 fallback 并返回 false ——
// 调用方据此打日志, 而不是默默用一个猜出来的值。
func smokeInt(key string, fallback int) (int, bool) {
	s := smokeCfg[key]
	if s == "" {
		return fallback, true
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return fallback, false
	}
	return n, true
}

// bridgeBinOverride 指定用哪份已安装的桥二进制; 空表示从工作树现编。
func bridgeBinOverride() string { return smokeCfg["ZCODE_ACP_BRIDGE_BIN"] }

// taskDir / taskPromptFallback 是 TestSmokeRealTask 的入参。命令行 flag 优先
// (见 task_test.go 的 firstSet), 这里给文件这一层。
func taskDir() string            { return smokeCfg["ZCODE_SMOKE_PROJECT_DIR"] }
func taskPromptFallback() string { return smokeCfg["ZCODE_SMOKE_PROMPT"] }

// taskTimeoutMinutes 是真机用例的等待上限 (分钟)。配了非法值时退回默认并告警。
func taskTimeoutMinutes(def int) (int, bool) {
	return smokeInt("ZCODE_SMOKE_TASK_TIMEOUT_MIN", def)
}

// 下面这组是 plan 审批 / 工具权限用例的输入。
func planApprovalPrompt() string  { return smokeCfg["ZCODE_ACP_SMOKE_PLAN_PROMPT"] }
func permissionOption() string    { return smokeCfg["ZCODE_ACP_SMOKE_PERMISSION_OPTION"] }
func permissionPrompt() string    { return smokeCfg["ZCODE_ACP_SMOKE_PERMISSION_PROMPT"] }
func permissionWorkspace() string { return smokeCfg["ZCODE_ACP_SMOKE_PERMISSION_WORKSPACE"] }
func askPrompt() string           { return smokeCfg["ZCODE_SMOKE_ASK_PROMPT"] }
func askWorkspace() string        { return smokeCfg["ZCODE_SMOKE_ASK_WORKSPACE"] }
