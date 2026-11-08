package bridge

import "testing"

// TestPermissionToolKind: 权限请求的 kind 按工具类型映射 ACP ToolKind 闭集,
// 让「读文件」与「跑命令」在授权弹窗里可区分 (改造 P1)。认不出的工具回 "other"
// —— 权限场景未知比瞎猜安全, 与 tool_call 展示翻译的兜底 "execute" 刻意不同。
func TestPermissionToolKind(t *testing.T) {
	cases := []struct {
		toolName string
		want     string
	}{
		{"Bash", "execute"},
		{"bash", "execute"},
		{"Edit", "edit"},
		{"Write", "edit"},
		{"Read", "read"},
		{"Grep", "search"},
		{"Glob", "search"},
		{"WebFetch", "fetch"},
		{"WebSearch", "fetch"},
		{"未知的工具", "other"},
		{"", "other"},
	}
	for _, c := range cases {
		if got := permissionToolKind(c.toolName); got != c.want {
			t.Errorf("permissionToolKind(%q) = %q, want %q", c.toolName, got, c.want)
		}
	}
}
