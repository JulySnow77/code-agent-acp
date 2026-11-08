//go:build real

// Package smoke 的真机参数装载。
//
// 真机用例需要 5 个参数才能真正跑起来 (ZCODE_BIN / 两个 provider 配置 /
// ZCODE_ACP_MODEL / ZCODE_ACP_BRIDGE_BIN)。以前靠 shell export 喂进来 —— 每次
// 重跑都要先把那五行从聊天记录里翻出来, 漏一行测试就静默 Skip, 报的是
// 「ZCODE_BIN 未设置」而不是真正的原因。
//
// 现在参数写在**项目根的 env** 里, TestMain 在跑任何用例之前把它读出来
// Setenv 到测试进程, 子进程serve 通过 os.Environ() 继承。现有用例的
// os.Getenv 一行都不用改, 就全部改从 env 取值。
//
// 使用:
//
//	cp env.example env    # 按本机路径填
//	go test -count=1 -tags=real ./test/smoke -run '^TestSmokeRealBuildTask$' -v
//
// 覆盖/查找规则见 LoadSmokeEnv。
package smoke

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// envFileVar 指向 env 的位置。它是唯一还需要从环境拿的东西 —— 它描述的是
// 「配置在哪」, 不是配置本身, 只能靠环境传。
const envFileVar = "ZCODE_SMOKE_ENV_FILE"

// keepExistingVar=1 时进程里已有的同名环境变量不被 env 覆盖。默认反过来:
// env 是唯一事实来源, 免得某次遗留的 export 悄悄改了被测配置而日志上
// 看不出来 (覆盖动作会打日志)。
const keepExistingVar = "ZCODE_SMOKE_ENV_KEEP_EXISTING"

// envFileNames 按优先级排列, 同级目录里 env.local 先于 env。
var envFileNames = []string{"env.local", "env"}

// requiredKeys 是跑一个真机用例至少要有的参数; 缺它们现有用例会 Skip。
//
// 这四个都**必须**进环境: 前两个 zcode 子进程要读, 后两个桥进程自己
// os.Getenv (见 internal/compat/env.go)。测试和它们之间隔着进程边界,
// 环境是唯一通道。
var requiredKeys = []string{
	"ZCODE_BIN",
	"ZCODE_BUILTIN_PROVIDER_CONFIG_FILE",
	"ZCODE_PERSONAL_PROVIDER_CONFIG_FILE",
	"ZCODE_ACP_MODEL",
}

// bridgeOnlyKeys 是桥进程会 os.Getenv、但真机用例用不到的可选项。
// 它们仍然要 Setenv —— 桥是独立进程, 测试没法用别的方式告诉它。
var bridgeOnlyKeys = []string{
	"ZCODE_ACP_MODE",
	"ZCODE_ACP_SURFACE",
	"ZCODE_ACP_VERBOSE",
}

// optionalKeys 是**测试侧**配置: 只影响这个测试进程自己怎么跑, 桥和 zcode 都不
// 读。它们只进 smokeCfg (见 smoke_config.go), 不进 os.Environ()。
//
// 分成两处的理由: 让「谁真正消费了这个值」一眼可辨 —— 留在 requiredKeys /
// bridgeOnlyKeys 里的要跨进程, 这里的出不了进程边界, 塞进环境只会让子进程
// 拿到一堆没人读的值。
//
// 设 1 时 harness 不再自动应答权限/提问请求, 而是把问题+选项打到 stderr、
// 从 stdin 读人的选择 —— 用来手动验证 AskUserQuestion 这类「要人答」的流程。
var optionalKeys = []string{
	"ZCODE_ACP_BRIDGE_BIN",
	// 下面这两个是 TestSmokeRealTask 的全部入参 —— 「在哪个目录干活 + 干什么活」。
	// 命令行写法 (-args -task.dir=... -task.prompt=...) 优先级更高。
	"ZCODE_SMOKE_PROJECT_DIR",
	"ZCODE_SMOKE_PROMPT",
	"ZCODE_SMOKE_TASK_TIMEOUT_MIN",
	"ZCODE_SMOKE_ASK_PROMPT",
	"ZCODE_SMOKE_ASK_WORKSPACE",
	"ZCODE_SMOKE_ASK_FALLBACK",
	"ZCODE_ACP_SMOKE_PLAN_PROMPT",
	"ZCODE_ACP_SMOKE_PERMISSION_OPTION",
	"ZCODE_ACP_SMOKE_PERMISSION_PROMPT",
	"ZCODE_ACP_SMOKE_PERMISSION_WORKSPACE",
	"ZCODE_SMOKE_INTERACTIVE",
}

// EnvReport 记录这一次装载做了什么 —— 测试开头那几行输出就是它, 排查
// 「为什么这个用例 Skip 了」时先看这个。
type EnvReport struct {
	// File 是实际读取的文件路径, 空表示没找到任何候选文件。
	File string
	// Tried 是被找过但不存在的候选路径, 按查找顺序。
	Tried []string
	// Loaded 是成功注入的 KEY=VALUE。
	Loaded []string
	// Kept 是进程里已存在、按 keepExisting 规则未被覆盖的 KEY=VALUE=现值。
	Kept []string
	// Missing 是**已知需要**但装载后仍然没有值的 KEY。
	Missing []string
}

// LoadSmokeEnv 找到 env, 解析后 Setenv 到当前进程。
//
// 查找顺序:
//  1. ZCODE_SMOKE_ENV_FILE 指定的路径 (唯一允从环境传的参数);
//  2. 从当前目录逐级向上找 env.local / env —— go test 的 cwd 是包目录
//     (test/smoke), 向上两级就是项目根。
//
// 找不到文件不算错: 返回 FileMissing 的报告, 用例照旧按 os.Getenv 为空 Skip,
// 报告里会把找过的路径列出来。
func LoadSmokeEnv() EnvReport {
	var rep EnvReport
	rep.File, rep.Tried = findEnvFile()
	if rep.File == "" {
		return rep
	}
	vars, err := parseDotEnvFile(rep.File)
	if err != nil {
		// 文件找到了但读不出内容 —— 这是配置错误, 不是「没配」, 必须吵出来。
		panic(fmt.Sprintf("读取 %s 失败: %v", rep.File, err))
	}

	keep := os.Getenv(keepExistingVar) == "1"
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// 测试侧配置与「要跨进程的」分开收: 前者只进 smokeCfg, 后者才 Setenv。
	fromFile := map[string]string{}
	keptFromEnv := map[string]string{}
	for _, k := range keys {
		v := vars[k]
		if keep {
			if cur, exists := os.LookupEnv(k); exists {
				rep.Kept = append(rep.Kept, fmt.Sprintf("%s=%s", k, cur))
				keptFromEnv[k] = cur
				continue
			}
		}
		rep.Loaded = append(rep.Loaded, fmt.Sprintf("%s=%s", k, v))
		fromFile[k] = v
		// 只有桥/zcode 真正会 os.Getenv 的键才进进程环境。测试侧配置留在
		// smokeCfg 里, 免得随 os.Environ() 继承给子进程、却没有读者。
		if isBridgeEnvKey(k) {
			if err := os.Setenv(k, v); err != nil {
				panic(fmt.Sprintf("Setenv %s 失败: %v", k, err))
			}
		}
	}
	setSmokeConfig(fromFile, keptFromEnv)

	for _, k := range append(append(append([]string{}, requiredKeys...), bridgeOnlyKeys...), optionalKeys...) {
		if os.Getenv(k) == "" && smokeCfg[k] == "" {
			rep.Missing = append(rep.Missing, k)
		}
	}
	return rep
}

// isBridgeEnvKey 判断一个键要不要进进程环境。
//
// 白名单而非黑名单: 新加的键默认**不**进环境, 要进必须显式列进来。这样「某个值
// 到底有没有跨进程」永远是白名单上的一次显式决定, 而不会因为文件里多写了一行
// 就悄悄把一堆没人读的值塞进子进程环境。
func isBridgeEnvKey(k string) bool {
	for _, key := range append(append([]string{}, requiredKeys...), bridgeOnlyKeys...) {
		if k == key {
			return true
		}
	}
	return false
}

// FileMissing 报告这一轮有没有找到配置文件。
func (r EnvReport) FileMissing() bool { return r.File == "" }

// KeepExisting 复述 LoadSmokeEnv 用的那条优先级规则, 供断言区分「值被保留」
// 与「值被覆盖」。
func (r EnvReport) KeepExisting() bool { return os.Getenv(keepExistingVar) == "1" }

// Report 生成人类可读的装载摘要, 供 TestMain 打印。
func (r EnvReport) Report() string {
	var b strings.Builder
	switch {
	case r.File == "":
		fmt.Fprintf(&b, "smoke 环境: 未找到 %s (%s), 已按顺序查找:\n",
			strings.Join(envFileNames, " / "), "真机用例会 Skip")
		for _, p := range r.Tried {
			fmt.Fprintf(&b, "    ✗ %s\n", p)
		}
		fmt.Fprintf(&b, "  → cp env.example %s 后填上本机路径\n", filepath.Join(projectRoot(), "env"))
	default:
		fmt.Fprintf(&b, "smoke 环境: %s (%d 项)\n", r.File, len(r.Loaded))
		// 标明每个值的去向。两者只有这一个区别: 桥/zcode 环境里的会随
		// os.Environ() 进子进程, 仅测试进程里的不会 —— 排查「这个参数到底
		// 有没有传下去」时, 这一行就是答案。
		for _, kv := range r.Loaded {
			scope := "仅测试进程"
			if k, _, ok := strings.Cut(kv, "="); ok && isBridgeEnvKey(k) {
				scope = "桥/zcode 环境"
			}
			fmt.Fprintf(&b, "    ✓ %s [%s]\n", kv, scope)
		}
	}
	for _, kv := range r.Kept {
		fmt.Fprintf(&b, "    - %s (进程里已有, %s=1 未覆盖)\n", kv, keepExistingVar)
	}
	if len(r.Missing) > 0 {
		fmt.Fprintf(&b, "    ⚠ 未设置: %s\n", strings.Join(r.Missing, ", "))
	}
	return b.String()
}

// findEnvFile 定位 env: 先看 envFileVar, 再从 cwd 逐级向上找。
func findEnvFile() (string, []string) {
	var tried []string
	if p := os.Getenv(envFileVar); p != "" {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, tried
		}
		tried = append(tried, p)
		// 显式指定了却读不到 —— 别悄悄退化到向上查找, 那会让人以为改了生效。
		return "", tried
	}

	dir, err := os.Getwd()
	if err != nil {
		return "", tried
	}
	for {
		for _, name := range envFileNames {
			p := filepath.Join(dir, name)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p, tried
			}
			tried = append(tried, p)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", tried
}

// projectRoot 从 cwd 向上找到含 go.mod 的目录, 找不到时用 cwd —— 只为给
// 「cp env.example 到哪里」一个准确路径。
func projectRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir
		}
		dir = parent
	}
}

// parseDotEnvFile 解析 env 成 map。
//
// 支持的形状 (够用就好的子集, 对齐 dotenv 惯例):
//
//	KEY=value                普通键值
//	export KEY=value         shell 写法, 前缀忽略
//	KEY="带 空格 的 值"        双引号保留空白, 处理 \n \t \" 转义, 支持行尾注释
//	KEY='原样 $NOT_EXPANDED'   单引号不做转义、不展开变量
//	KEY=value # 行尾注释       裸值遇空格+ # 才当注释 (/Applications/... 里没
//	                         空格带 # 的情况很罕见, 这样最不容易误伤路径)
//	KEY=${OTHER}/tail         引用前面定义过的变量或进程里已有的变量
//
// 空行、# 开头的整行注释、缺 `=` 的行: 跳过。
func parseDotEnvFile(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	vars := map[string]string{}
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	for i, line := range lines {
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		s = strings.TrimPrefix(s, "export ")
		eq := strings.Index(s, "=")
		if eq <= 0 {
			continue // 没有 = 的行不做猜测, 静默跳过
		}
		key := strings.TrimSpace(s[:eq])
		value := strings.TrimLeft(s[eq+1:], " \t")
		if key == "" {
			return nil, fmt.Errorf("第 %d 行缺少键名: %q", i+1, line)
		}
		parsed, err := parseDotEnvValue(value, vars)
		if err != nil {
			return nil, fmt.Errorf("第 %d 行 (%s): %w", i+1, key, err)
		}
		vars[key] = parsed
	}
	return vars, nil
}

// parseDotEnvValue 按引号形式解析值, 并对双引号/裸值做变量展开。
func parseDotEnvValue(s string, vars map[string]string) (string, error) {
	switch {
	case strings.HasPrefix(s, "'"):
		end := strings.Index(s[1:], "'")
		if end < 0 {
			return "", fmt.Errorf("单引号未闭合")
		}
		return s[1 : 1+end], nil

	case strings.HasPrefix(s, "\""):
		var b strings.Builder
		for i := 1; i < len(s); i++ {
			c := s[i]
			switch {
			case c == '\\' && i+1 < len(s):
				i++
				switch s[i] {
				case 'n':
					b.WriteByte('\n')
				case 't':
					b.WriteByte('\t')
				case 'r':
					b.WriteByte('\r')
				case '"', '\\', '$':
					b.WriteByte(s[i])
				default:
					b.WriteByte('\\')
					b.WriteByte(s[i])
				}
			case c == '"':
				return expandDotEnv(b.String(), vars), nil
			default:
				b.WriteByte(c)
			}
		}
		return "", fmt.Errorf("双引号未闭合")

	default:
		// 裸值: 只在「空白 + #」处切断注释 —— 路径里的 # 不该被吃掉。
		if idx := indexComment(s); idx >= 0 {
			s = s[:idx]
		}
		return expandDotEnv(strings.TrimRight(s, " \t"), vars), nil
	}
}

// indexComment 找裸值里行尾注释的起点, 没有则返回 -1。
func indexComment(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] != '#' {
			continue
		}
		if i == 0 || s[i-1] == ' ' || s[i-1] == '\t' {
			return i
		}
	}
	return -1
}

// expandDotEnv 展开 ${VAR} 与 $VAR: 先查本文件已解析的前面的行, 再查进程环境。
// 查不到的保持原样 —— 把 ${MISSING} 静默变空串, 排查起来更难受。
func expandDotEnv(s string, vars map[string]string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '$' {
			b.WriteByte(s[i])
			continue
		}
		name, length := readVarName(s[i+1:])
		if name == "" {
			b.WriteByte(s[i])
			continue
		}
		if v, ok := vars[name]; ok {
			b.WriteString(v)
		} else if v, ok := os.LookupEnv(name); ok {
			b.WriteString(v)
		} else {
			b.WriteString(s[i : i+1+length])
		}
		i += length
	}
	return b.String()
}

// readVarName 读 ${NAME} 或 NAME, 返回名字与它在 s 里占的长度 (不含 ${} 时也为
// 上层计算跳过位置用)。
func readVarName(s string) (string, int) {
	if s == "" {
		return "", 0
	}
	if s[0] == '{' {
		end := strings.Index(s, "}")
		if end < 0 {
			return "", 0
		}
		name := s[1:end]
		if name == "" || strings.ContainsAny(name, " \t") {
			return "", 0
		}
		return name, end + 1
	}
	end := 0
	for end < len(s) && (isVarChar(s[end])) {
		end++
	}
	if end == 0 {
		return "", 0
	}
	return s[:end], end
}

func isVarChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
		return true
	}
	return false
}
