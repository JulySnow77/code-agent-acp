//go:build real

package smoke

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestMain 在跑任何真机用例之前把 env 装载进测试进程。
//
// 为什么必须在 TestMain 而不是各用例里 lazy load: 子进程 serve 通过
// os.Environ() 继承环境, 装载晚于 exec.Command 就漏参数; 而且实测希望在
// 第一个用例之前就把「这次用的是哪份配置」打出来 —— Skip 时看这一屏就知道
// 缺什么, 不用翻 History 找那五行 export。
func TestMain(m *testing.M) {
	// 先收孤儿锁再起后端 —— 顺序反了就白清: 后端一旦启动就会重建自己的锁。
	fmt.Print(cleanStaleLocks().Report())
	fmt.Print(LoadSmokeEnv().Report())
	os.Exit(m.Run())
}

// TestSmokeEnvFromFile 证的就是「参数来自 env, 而不是靠 export」这件事本身。
//
// 它不连真机, 跑得很快, 可以单独执行:
//
//	go test -count=1 -tags=real ./test/smoke -run TestSmokeEnvFromFile -v
//
// 断言三件事:
//  1. 找到了配置文件 (没找到就说明前置没做, 不是「通过」);
//  2. env 里写的每个 KEY 在进程里都能读到同名同值 —— 这是 os.Setenv 真的
//     生效了的证据, 也是后面所有用例 os.Getenv 能拿到的保证;
//  3. 必需的 5 个参数都在, 缺谁列谁。
//
// 用例永远不依赖 shell 里残留的 export: 想验「没有 export 也能跑」, 就在干净
// shell 里执行上面的命令。
func TestSmokeEnvFromFile(t *testing.T) {
	rep := LoadSmokeEnv() // 已由 TestMain 装载过; 再调一次是幂等的, 顺便拿到报告。

	if rep.FileMissing() {
		t.Fatalf("没有找到 %s, 参数无处可来 (这些用例要求参数来自文件, 不接受 shell export)。\n%s\n"+
			"先执行: cp env.example env (然后按本机路径填写)",
			strings.Join(envFileNames, " / "), rep.Report())
	}
	t.Logf("配置文件: %s", rep.File)

	// 重新解析一次文件拿到期望值 —— 不能直接报告里的 Loaded 比对: 那是用 Mother
	// LoadSmokeEnv 自己的输出验自己, 空跑也会过。这里以文件原文为准。
	want, err := parseDotEnvFile(rep.File)
	if err != nil {
		t.Fatalf("重新解析 %s 失败: %v", rep.File, err)
	}
	if len(want) == 0 {
		t.Fatalf("%s 里没有任何 KEY=VALUE —— 参照 env.example 填写", rep.File)
	}

	// 按键的性质分别断言 —— 这是本用例的核心。env 文件里的键分两类, 走两条
	// 不同的路, 断言必须跟着分:
	//
	//   - 桥/zcode 要读的 (requiredKeys + bridgeOnlyKeys): 必须真的进了进程环境,
	//     否则子进程拿不到 —— 而子进程只能靠环境拿到;
	//   - 纯测试侧配置: 必须**不**进进程环境。它们出不了进程边界, 塞进去只会
	//     让 os.Environ() 多带一批没人读的值, 反而看不清谁消费了什么。
	//
	// 旧实现只有第一种断言 (「文件里写了就 os.Getenv 得到」), 所以「测试侧配置
	// 也被注入环境」这件事一直没人盯。
	for _, k := range sortedKeys(want) {
		fileVal := want[k]
		_, inEnv := os.LookupEnv(k)

		if isBridgeEnvKey(k) {
			got, ok := os.LookupEnv(k)
			if !ok {
				t.Errorf("%s 写在 %s 里, 但进程环境里读不到 —— 桥/zcode 子进程会缺这个参数",
					k, filepathBase(rep.File))
				continue
			}
			if rep.KeepExisting() {
				continue // 那种模式下交由报告明示保留, 不做值断言
			}
			if got != fileVal {
				t.Errorf("%s = %q, want %q (来自 %s)", k, got, fileVal, filepathBase(rep.File))
				continue
			}
			t.Logf("  ✓ %s=%s (桥/zcode 环境)", k, got)
			continue
		}

		// 测试侧: 值要能在 smokeCfg 里读到, 且不得出现在进程环境里。
		if got := smokeStr(k); got != fileVal && !rep.KeepExisting() {
			t.Errorf("测试侧配置 %s = %q, want %q", k, got, fileVal)
			continue
		}
		if inEnv {
			t.Errorf("%s 是测试侧配置, 却被注入进了进程环境 —— 它出不了进程边界, "+
				"只会被子进程原样继承却没人读", k)
			continue
		}
		t.Logf("  ✓ %s=%s (仅测试进程)", k, fileVal)
	}

	var missing []string
	for _, k := range requiredKeys {
		if os.Getenv(k) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		t.Errorf("必需参数未设置: %v —— 配套用例会因此 Skip (不是失败, 但也什么都没验到)", missing)
	}
}

// sortedKeys 只为让日志顺序稳定 —— env 的行序不该影响输出可读性。
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// filepathBase 是 filepath.Base 的具名包装, 报错里用它避免把整条绝对路径
// 铺满屏幕。
func filepathBase(p string) string { return filepath.Base(p) }

// TestIsBridgeEnvKey 白名单边界: 测试侧配置**默认不进**进程环境。
//
// 用白名单而不是黑名单是刻意的: env 文件里多写一行, 不该顺手把一个没人读的
// 值带进子进程环境。「要不要跨进程」必须每次都是一次显式决定。
func TestIsBridgeEnvKey(t *testing.T) {
	for _, k := range []string{
		"ZCODE_BIN",
		"ZCODE_ACP_MODEL",
		"ZCODE_BUILTIN_PROVIDER_CONFIG_FILE",
		"ZCODE_PERSONAL_PROVIDER_CONFIG_FILE",
		"ZCODE_ACP_MODE",
	} {
		if !isBridgeEnvKey(k) {
			t.Errorf("%s 是桥/zcode 要读的, 应进进程环境", k)
		}
	}
	for _, k := range []string{
		"ZCODE_ACP_BRIDGE_BIN",
		"ZCODE_SMOKE_INTERACTIVE",
		"ZCODE_SMOKE_PROMPT",
		"ZCODE_ACP_SMOKE_PERMISSION_OPTION",
		"ZCODE_ACP_BRIDGE_BIN_NEWLY_ADDED",
		"PATH", "HOME",
	} {
		if isBridgeEnvKey(k) {
			t.Errorf("%s 没有跨进程读者, 不该进进程环境", k)
		}
	}
}
