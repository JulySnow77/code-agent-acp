//go:build real

package smoke

import (
	"bytes"
	"strings"
	"testing"
)

// 这一组用例钉的是「人在终端里到底看到了什么」。
//
// 背景: 交互模式第一版把选项画成了空的 "[1] [2] [3] [4]" —— 因为按
// label/value 取字段, 而桥投放的是 optionId/name (bridge/ask.go 的 askOptions、
// bridge/permission.go 的 _meta), 问题原文也不在 toolCall.question 而在
// toolCall.rawInput.questions[].question。这种 bug 跑真机才看得见, 而且一眼会
// 误判成「模型/桥没给选项」, 所以在这里用桥的真实形状兜住。

// TestRenderAskPromptShowsLabelsAndQuestion 验证降级路径的 AskUserQuestion
// 能把问题原文 + 每个选项的 label 显示出来, 而不是一串空的 "[n] "。
func TestRenderAskPromptShowsLabelsAndQuestion(t *testing.T) {
	params := askFixture()
	var buf bytes.Buffer
	renderPermissionPrompt(&buf, params)
	got := buf.String()

	for _, want := range []string{
		"标题: AskUserQuestion",
		"问题: 你希望我按哪种侧重点来总结本项目？",
		"[1] 技术栈视角",
		"[2] 业务/产品视角",
		"[3] 团队协作与仓库结构",
		"[4] Cancel", // 桥追加的取消项
	} {
		if !strings.Contains(got, want) {
			t.Errorf("提示里缺 %q\n整个提示:\n%s", want, got)
		}
	}
	// 空选项行的特征: "[n] " 后面直接换行。曾经整整一排都是这样。
	for _, line := range strings.Split(got, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(line, "] ") {
			t.Errorf("出现了空选项行: %q\n整个提示:\n%s", line, got)
		}
	}
	// 取消项的 optionId 含 NUL (bridge/ask.go 的 askCancelOptionID), 不能吐到终端。
	if strings.Contains(got, "\x00") {
		t.Errorf("提示里出现了 NUL (取消项的 optionId 应被过滤)\n整个提示:\n%s", got)
	}
	t.Logf("人在终端里看到的是:\n%s", got)
}

// TestRenderToolPermissionShowsDescription 验证普通工具授权也能看懂: 有 name,
// 且能从 _meta.zcode.options 里补出 zcode 给的 description。
func TestRenderToolPermissionShowsDescription(t *testing.T) {
	params := map[string]any{
		"sessionId": "s1",
		"toolCall": map[string]any{
			"toolCallId": "call-1",
			"title":      "Bash",
			"name":       "Bash",
			"kind":       "other",
			"rawInput":   map[string]any{"command": "ls -la"},
		},
		"options": []any{
			map[string]any{"optionId": "allow-once", "name": "Allow once", "kind": "allow_once"},
			map[string]any{"optionId": "allow-always", "name": "Always allow", "kind": "allow_always"},
		},
		"_meta": map[string]any{"zcode": map[string]any{
			"input": map[string]any{"command": "ls -la"},
			"options": map[string]any{
				"allow-once":   map[string]any{"kind": "allowOnce", "description": "仅本次允许 ls -la"},
				"allow-always": map[string]any{"kind": "allowAlways", "description": "本会话免除确认"},
			},
		}},
	}
	var buf bytes.Buffer
	renderPermissionPrompt(&buf, params)
	got := buf.String()

	for _, want := range []string{
		"标题: Bash",
		"[1] Allow once",
		"仅本次允许 ls -la",
		"[2] Always allow",
		"本会话免除确认",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("提示里缺 %q\n整个提示:\n%s", want, got)
		}
	}
}

// askFixture 按 bridge/ask.go 的 askOne 造一条真实的 session/request_permission:
// options 是 {optionId: value, name: label, kind}, 问题原文在 rawInput.questions。
func askFixture() map[string]any {
	question := map[string]any{
		"question":    "你希望我按哪种侧重点来总结本项目？",
		"header":      "侧重点",
		"multiSelect": false,
		"options": []any{
			map[string]any{"label": "技术栈视角", "value": "tech",
				"description": "语言、框架、依赖"},
			map[string]any{"label": "业务/产品视角", "value": "biz",
				"description": "面向的用户与场景"},
			map[string]any{"label": "团队协作与仓库结构", "value": "repo",
				"description": "多人协作方式与目录划分"},
		},
	}
	return map[string]any{
		"sessionId": "s1",
		"toolCall": map[string]any{
			"toolCallId": "call-1",
			"title":      "AskUserQuestion",
			"kind":       "other",
			"rawInput":   map[string]any{"questions": []any{question}},
		},
		"options": []any{
			map[string]any{"optionId": "tech", "name": "技术栈视角", "kind": "allow_once"},
			map[string]any{"optionId": "biz", "name": "业务/产品视角", "kind": "allow_once"},
			map[string]any{"optionId": "repo", "name": "团队协作与仓库结构", "kind": "allow_once"},
			// 桥追加的取消项: optionId 含 NUL (bridge/ask.go 的 askCancelOptionID)
			map[string]any{"optionId": "\x00zcode:ask-cancel", "name": "Cancel", "kind": "reject_once"},
		},
	}
}
