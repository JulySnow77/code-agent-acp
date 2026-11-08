package bridge

import (
	"testing"

	"github.com/tizerluo/zcode-open-bridge-go/internal/zcode"
)

// askDedupKey 的边界: 只有 zcode 的 requestId / toolCallId 能当去重键, 两者都缺
// 时必须返回空 —— 宁可多问一次, 也不能把两道不同的题误认成一道。
func TestAskDedupKey(t *testing.T) {
	for _, tc := range []struct {
		name   string
		params map[string]any
		want   string
	}{
		{"requestId 优先", map[string]any{"requestId": "r1", "toolCallId": "t1"}, "r1"},
		{"缺 requestId 时退到 toolCallId", map[string]any{"toolCallId": "t1"}, "t1"},
		{"两个都没有 → 不去重", map[string]any{"sessionId": "s"}, ""},
		{"空串等同缺失", map[string]any{"requestId": "", "toolCallId": ""}, ""},
		// zcode.Str 会把数字 widen 成字符串。这是好的: 数字 requestId widen 后
		// 仍是跨重发稳定的键, 去重照常成立 (zcode 实际发的是字符串)。
		{"数字 requestId 被 widen 成交互稳定的键", map[string]any{"requestId": 42.0}, "42"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := askDedupKey(&zcode.ServerRequest{Params: tc.params})
			if got != tc.want {
				t.Errorf("askDedupKey = %q, want %q", got, tc.want)
			}
		})
	}
}

// askDedup.put 空键不入表 —— 否则「没有可靠键」的请求会把彼此当成重发。
func TestAskDedupIgnoresEmptyKey(t *testing.T) {
	d := newAskDedup()
	d.put("", map[string]any{"action": "accept"})
	if _, ok := d.seen(""); ok {
		t.Error("空键不应被记住, 否则两道无 requestId 的题会互相吞掉")
	}

	d.put("r1", map[string]any{"action": "accept"})
	got, ok := d.seen("r1")
	if !ok || got["action"] != "accept" {
		t.Errorf("r1 应记住上一份应答, 实际 %v (ok=%v)", got, ok)
	}
	if _, ok := d.seen("r2"); ok {
		t.Error("r2 是另一个键, 不该命中")
	}
}
