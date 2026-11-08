package zcode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// loadRecordedCreate 加载 0.16.5 实录的 session/create 响应 (协议实录回归,
// PORTING_PLAN §7: 每个 testdata/zcode-<ver>/ 样本都要跑一遍解析器)。
func loadRecordedCreate(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "zcode-0.16.5", "session-create.response.json"))
	if err != nil {
		t.Fatalf("读实录样本: %v", err)
	}
	var msg map[string]any
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("解析实录样本: %v", err)
	}
	// 实录文件存的是 result 内容本身 (无外层信封); 若带信封则剥开。
	if result, ok := msg["result"].(map[string]any); ok {
		return result
	}
	if _, has := msg["settings"]; !has {
		t.Fatal("实录样本既无 result 信封也无顶层 settings")
	}
	return msg
}

// TestParseModelCatalogRecorded 用 0.16.5 实录验证目录解析。重点回归:
// reasoning.levels 是 [{label,value}] 对象数组而非字符串数组, 按字符串解析
// 会得到空列表, 让 setModel 因 "Reasoning level is required" 被拒。
func TestParseModelCatalogRecorded(t *testing.T) {
	cat := ParseModelCatalog(loadRecordedCreate(t))
	if cat.Empty() {
		t.Fatal("实录响应解析出空目录")
	}
	if len(cat.Entries) != 19 {
		t.Errorf("目录条数 = %d, want 19 (实录 0.16.5)", len(cat.Entries))
	}

	const glmProvider = "a45187b5-dbbc-4342-a6c6-a8c9d42c9304"
	e := cat.Find(glmProvider, "glm-5.2")
	if e == nil {
		t.Fatalf("目录中找不到 %s/glm-5.2", glmProvider)
	}
	if e.DefaultLevel != "max" {
		t.Errorf("glm-5.2 defaultLevel = %q, want max", e.DefaultLevel)
	}
	if !reflect.DeepEqual(e.Levels, []string{"disabled", "high", "max"}) {
		t.Errorf("glm-5.2 levels = %v, want [disabled high max] (回归: {label,value} 对象数组)", e.Levels)
	}
	if e.ContextWindow <= 0 {
		t.Errorf("glm-5.2 contextWindow = %d, 应为正数", e.ContextWindow)
	}
}

// TestModelCatalogResolve 验证 ZCODE_ACP_MODEL 的三种 spec 形态解析。
func TestModelCatalogResolve(t *testing.T) {
	cat := ParseModelCatalog(loadRecordedCreate(t))

	ref, err := cat.Resolve("a45187b5-dbbc-4342-a6c6-a8c9d42c9304/glm-5.2")
	if err != nil || ref.ModelID != "glm-5.2" || ref.ProviderID != "a45187b5-dbbc-4342-a6c6-a8c9d42c9304" {
		t.Errorf("完整 spec 解析 = %+v, %v", ref, err)
	}

	// 只给 modelId: 目录内唯一匹配 (glm-5.2 只出现在一个 provider 下)。
	ref, err = cat.Resolve("glm-5.2")
	if err != nil || ref.ModelID != "glm-5.2" {
		t.Errorf("裸 modelId 解析 = %+v, %v", ref, err)
	}

	// gpt-5.5 挂在两个 provider 下 → 歧义必须报错而不是随机选一个。
	if _, err := cat.Resolve("gpt-5.5"); err == nil {
		t.Error("歧义 modelId 应报错")
	}

	if _, err := cat.Resolve("nope/nope"); err == nil {
		t.Error("不在目录的模型应报错")
	}
	if _, err := cat.Resolve("  "); err == nil {
		t.Error("空 spec 应报错")
	}
}

// TestDefaultReasoningLevel 验证缺省推理等级: defaultLevel 优先, 缺失时取
// 第一个可用等级 (0.16.5 对部分模型强制要求 reasoningLevel)。
func TestDefaultReasoningLevel(t *testing.T) {
	cat := ParseModelCatalog(loadRecordedCreate(t))
	glm := ModelRef{ProviderID: "a45187b5-dbbc-4342-a6c6-a8c9d42c9304", ModelID: "glm-5.2"}
	if got := cat.DefaultReasoningLevel(glm); got != "max" {
		t.Errorf("glm-5.2 缺省等级 = %q, want max (defaultLevel)", got)
	}
	if got := cat.DefaultReasoningLevel(ModelRef{ModelID: "ghost"}); got != "" {
		t.Errorf("目录外模型应返回空, got %q", got)
	}
	var nilCat *ModelCatalog
	if got := nilCat.DefaultReasoningLevel(glm); got != "" {
		t.Errorf("nil 目录应返回空, got %q", got)
	}
}

// TestParseModelCatalogShapes 覆盖目录形状的兜底分支: 缺 settings、字符串
// 形状的 levels (旧版)、平铺的 providerId/modelId。
func TestParseModelCatalogShapes(t *testing.T) {
	if c := ParseModelCatalog(map[string]any{}); !c.Empty() {
		t.Error("缺 settings 应得空目录而非错误")
	}

	cat := ParseModelCatalog(map[string]any{
		"settings": map[string]any{"model": map[string]any{"available": []any{
			// 旧版字符串 levels + 平铺 ref。
			map[string]any{"providerId": "p1", "modelId": "old-1", "reasoning": map[string]any{
				"levels": []any{"low", "high"},
			}},
			// 条目不是对象: 跳过不报错。
			"garbage",
		}}},
	})
	if len(cat.Entries) != 1 {
		t.Fatalf("条数 = %d, want 1", len(cat.Entries))
	}
	e := cat.Entries[0]
	if e.Ref.ProviderID != "p1" || e.Ref.ModelID != "old-1" {
		t.Errorf("平铺 ref 解析异常: %+v", e.Ref)
	}
	if !reflect.DeepEqual(e.Levels, []string{"low", "high"}) {
		t.Errorf("字符串 levels 解析 = %v", e.Levels)
	}
	if got := cat.DefaultReasoningLevel(e.Ref); got != "low" {
		t.Errorf("无 defaultLevel 应取第一个等级, got %q", got)
	}
}

func TestLevelValue(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{"high", "high"},
		{map[string]any{"label": "高", "value": "high"}, "high"},
		{map[string]any{"label": "high"}, "high"}, // 无 value 时回退 label
		{map[string]any{}, ""},
		{42, ""},
		{nil, ""},
	}
	for _, c := range cases {
		if got := levelValue(c.in); got != c.want {
			t.Errorf("levelValue(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
