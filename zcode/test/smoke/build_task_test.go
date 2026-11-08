//go:build real

package smoke

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSmokeRealBuildTask 在真实 ZCode 上跑一个完整的多步 build 模式任务:
// 修改文件 → 执行测试脚本 → 正常结束 (end_turn)。
//
// 前面的权限/问答用例各验一个转换点 (桥↔zcode 的应答形状); 这里验的是把
// 转换点串起来之后, 一整条 turn 能在 build 模式下稳定落地 —— 日常使用的
// 最小完整证明。客户端对每个权限请求自动回「允许一次」, 模型需要真的:
//  1. 用工具把 greeting.txt 的 hello 改成 HELLO;
//  2. 运行 sh check.sh (脚本先落 marker 再判定, 「口头跑测试」会露馅)。
//
// 参数从项目根的 env 读 (TestMain 装载), 不再需要 export —— 见 TestSmokeEnvFromFile。
// **-count=1 不能省** (换环境变量重跑可能命中缓存, 见 TestSmokeRealPermission 的说明)。
//
//	go test -count=1 -tags=real ./test/smoke -run TestSmokeRealBuildTask -v -timeout 15m
//
// 想在**自己的项目上跑自己的任务**, 用 TestSmokeRealTask: 它只吃两个参数
// (项目目录 + prompt), 本用例则是自带 fixture 的固定版本 —— 两者共用同一套
// 真机装置 (task_harness_test.go)。
func TestSmokeRealBuildTask(t *testing.T) {
	if os.Getenv("ZCODE_BIN") == "" {
		t.Skip("ZCODE_BIN 未设置 (真机任务验证需要)")
	}

	workspace := t.TempDir()
	writeFile := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(workspace, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("greeting.txt", "hello\n")
	// marker (check-ran.txt) 先于判定落盘: 断言「测试真的被执行过」不依赖
	// 从流式更新里解析工具输出 —— 后者形状会漂, 前者不会撒谎。
	writeFile("check.sh", `#!/bin/sh
echo ran > check-ran.txt
if grep -qx HELLO greeting.txt; then
  echo PASS
else
  echo FAIL
  exit 1
fi
`)

	prompt := "工作区里有两个文件: greeting.txt (内容是 hello) 和 check.sh。请完成下面两步, " +
		"每一步都必须真的调用工具, 不要只用文字回答:\n" +
		"1. 用工具把 greeting.txt 的内容改成 HELLO (就这一行)。\n" +
		"2. 用 Bash 运行 `sh check.sh`, 确认它输出 PASS。"

	res := runTask(t, TaskOptions{Workspace: workspace, Prompt: prompt})

	if res.PromptError != nil {
		t.Fatalf("session/prompt 返回错误: %v", res.PromptError)
	}
	t.Logf("工具调用 (%d): %s (工具输出含 PASS=%v)",
		len(res.ToolCalls), strings.Join(res.ToolCalls, " → "), strings.Contains(res.ToolOutput, "PASS"))
	if res.PermAnswered == 0 {
		t.Logf("ℹ 整个 turn 没有发生权限请求 (zcode 可能把工具判成了只读/低风险)")
	}
	if res.StopReason != "end_turn" {
		t.Errorf("stopReason = %s, want end_turn —— 任务没有正常结束", res.StopReason)
	}

	// 文件断言不经过模型自述, 直接看工作区状态。
	greeting, err := os.ReadFile(filepath.Join(workspace, "greeting.txt"))
	if err != nil {
		t.Fatalf("读 greeting.txt: %v", err)
	}
	if got := strings.TrimSpace(string(greeting)); got != "HELLO" {
		t.Errorf("greeting.txt 内容 = %q, want HELLO —— 模型没有真的改文件", got)
	}
	if _, err := os.Stat(filepath.Join(workspace, "check-ran.txt")); err != nil {
		t.Errorf("check-ran.txt 不存在 —— check.sh 没有真的被执行过")
	}
}

// pickAllowOnce 从权限请求的选项里挑「允许一次」: 日常 build 模式最常用的
// 回答。kind 是 ACP 闭集 (allow_once/allow_always/reject_once/...), 找不到
// allow_once 时退回第一个 kind 带 allow 的, 再不行退第一个 —— 总要回点什么,
// 悬着只会吃满超时。
func pickAllowOnce(params map[string]any) string {
	options, _ := params["options"].([]any)
	var first, firstAllow string
	for _, raw := range options {
		o, _ := raw.(map[string]any)
		id, _ := o["optionId"].(string)
		kind, _ := o["kind"].(string)
		if id == "" {
			continue
		}
		if first == "" {
			first = id
		}
		if firstAllow == "" && strings.HasPrefix(kind, "allow") {
			firstAllow = id
		}
		if kind == "allow_once" {
			return id
		}
	}
	if firstAllow != "" {
		return firstAllow
	}
	return first
}
