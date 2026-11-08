package compat

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tizerluo/zcode-open-bridge-go/internal/zcode"
)

// stubModels 是带单一模型的目录 stub, 控制 providerId 解析与缺省等级。
type stubModels struct {
	ref zcode.ModelRef
	lvl string
}

func (s *stubModels) Resolve(spec string) (zcode.ModelRef, error) {
	if spec == "glm" {
		return s.ref, nil
	}
	return zcode.ModelRef{}, &stubError{}
}

type stubError struct{}

func (*stubError) Error() string { return "not in catalog" }

func (s *stubModels) DefaultReasoningLevel(ref zcode.ModelRef) string {
	if ref == s.ref {
		return s.lvl
	}
	return ""
}

// TestRewriteSetModelUpgrade 覆盖 C4 的四条路径: 旧格式升格、新格式补缺省、
// 显式等级优先、无目录时的尽力拆分。
func TestRewriteSetModelUpgrade(t *testing.T) {
	models := &stubModels{
		ref: zcode.ModelRef{ProviderID: "prov-1", ModelID: "glm"},
		lvl: "max",
	}

	t.Run("旧格式 modelId 升格", func(t *testing.T) {
		orig := map[string]any{"sessionId": "s", "modelId": "glm"}
		r := Rewriter{Models: models}
		out, _, err := r.Rewrite("session/setModel", orig)
		if err != nil {
			t.Fatalf("升格失败: %v", err)
		}
		model, _ := out["model"].(map[string]any)
		if model == nil || model["providerId"] != "prov-1" || model["modelId"] != "glm" {
			t.Fatalf("model 升格结果异常: %v", out)
		}
		opts, _ := model["options"].(map[string]any)
		if opts["reasoningLevel"] != "max" {
			t.Errorf("缺省 reasoningLevel = %v, want max", opts)
		}
		if _, still := out["modelId"]; still {
			t.Error("旧 modelId 键应被移除")
		}
		// 浅拷贝: 调用方的原 map 不被就地改动。
		if _, untouched := orig["modelId"]; !untouched {
			t.Error("调用方原 map 被就地修改了")
		}
	})

	t.Run("新格式只补缺省", func(t *testing.T) {
		r := Rewriter{Models: models}
		out, _, err := r.Rewrite("session/setModel", map[string]any{
			"model": map[string]any{"providerId": "prov-1", "modelId": "glm"},
		})
		if err != nil {
			t.Fatal(err)
		}
		model := out["model"].(map[string]any)
		if model["options"].(map[string]any)["reasoningLevel"] != "max" {
			t.Errorf("应补缺省等级: %v", model)
		}
	})

	t.Run("显式 reasoningLevel 优先", func(t *testing.T) {
		r := Rewriter{Models: models}
		out, _, err := r.Rewrite("session/setModel", map[string]any{
			"modelId": "glm", "reasoningLevel": "disabled",
		})
		if err != nil {
			t.Fatal(err)
		}
		model := out["model"].(map[string]any)
		if got := model["options"].(map[string]any)["reasoningLevel"]; got != "disabled" {
			t.Errorf("显式等级被覆盖: %v", model)
		}
	})

	t.Run("已有等级不覆盖", func(t *testing.T) {
		r := Rewriter{Models: models}
		out, _, err := r.Rewrite("session/setModel", map[string]any{
			"model": map[string]any{
				"providerId": "prov-1", "modelId": "glm",
				"options": map[string]any{"reasoningLevel": "high"},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		model := out["model"].(map[string]any)
		if got := model["options"].(map[string]any)["reasoningLevel"]; got != "high" {
			t.Errorf("调用方显式给的等级被覆盖: %v", model)
		}
	})

	t.Run("无目录按斜杠拆分", func(t *testing.T) {
		r := Rewriter{}
		out, _, err := r.Rewrite("session/setModel", map[string]any{"modelId": "prov-9/m-9"})
		if err != nil {
			t.Fatal(err)
		}
		model := out["model"].(map[string]any)
		if model["providerId"] != "prov-9" || model["modelId"] != "m-9" {
			t.Errorf("无目录拆分异常: %v", model)
		}
	})

	t.Run("目录解析失败上抛", func(t *testing.T) {
		r := Rewriter{Models: models}
		if _, _, err := r.Rewrite("session/setModel", map[string]any{"modelId": "ghost"}); err == nil {
			t.Error("模型不在目录应报错 (doctor 提前暴露, 而非 zcode 拒绝)")
		}
	})
}

// TestRewriteFillSubscribe 覆盖 C6: subscribe 必填四字段, 已有值不覆盖。
func TestRewriteFillSubscribe(t *testing.T) {
	r := Rewriter{}
	out, _, err := r.Rewrite("session/subscribe", map[string]any{"sessionId": "s"})
	if err != nil {
		t.Fatal(err)
	}
	if out["deliveryKind"] != "desktop-continuous" || out["includeSnapshot"] != true || out["afterSeq"] != 0 {
		t.Errorf("subscribe 补字段异常: %v", out)
	}

	out, _, _ = r.Rewrite("session/subscribe", map[string]any{
		"sessionId": "s", "deliveryKind": "mobile", "afterSeq": 7,
	})
	if out["deliveryKind"] != "mobile" || out["afterSeq"] != 7 || out["includeSnapshot"] != true {
		t.Errorf("已有字段被覆盖: %v", out)
	}
}

// TestTimeouts 验证超时表 (C2 回归: create 默认 120s) 与 env 覆盖语义。
func TestTimeouts(t *testing.T) {
	d := DefaultTimeouts()
	if d.Create != 120*time.Second {
		t.Errorf("create 超时 = %s, want 120s (C2: 上游 15s 在 1.2GB 会话库下必超时)", d.Create)
	}
	if d.For("session/create") != d.Create || d.For("session/read") != d.Read {
		t.Error("For(method) 路由错误")
	}
	if d.For("session/unknown") != d.Default {
		t.Error("未知方法应落到 Default")
	}
	if d.Turn != 30*time.Minute || d.Question != 10*time.Minute {
		t.Errorf("turn/question 超时与上游约定不符: %s / %s", d.Turn, d.Question)
	}
}

func TestEnvDuration(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"90", 90 * time.Second}, // 裸数字按秒
		{"90s", 90 * time.Second},
		{"2m", 2 * time.Minute},
		{"bogus", 0},
		{"", 0},
	}
	for _, c := range cases {
		t.Setenv("ZCODE_ACP_TEST_DUR", c.env)
		if got := envDur("ZCODE_ACP_TEST_DUR"); got != c.want {
			t.Errorf("envDur(%q) = %s, want %s", c.env, got, c.want)
		}
	}
}

func TestTimeoutsFromEnv(t *testing.T) {
	t.Setenv("ZCODE_ACP_CREATE_TIMEOUT", "300")
	t.Setenv("ZCODE_ACP_TURN_TIMEOUT", "45m")
	got := TimeoutsFromEnv()
	if got.Create != 300*time.Second || got.Turn != 45*time.Minute {
		t.Errorf("env 覆盖无效: create=%s turn=%s", got.Create, got.Turn)
	}
	if got.Question != 10*time.Minute {
		t.Errorf("未覆盖的字段应保持默认: question=%s", got.Question)
	}
}

// TestRulesTableSanity 校验规则表自描述完整 (§5.2: 每条规则带样本与来源)。
func TestRulesTableSanity(t *testing.T) {
	ids := map[string]bool{}
	for _, r := range Rules {
		if r.ID == "" || r.Desc == "" || r.Sample == "" {
			t.Errorf("规则缺 ID/Desc/Sample: %+v", r)
		}
		if ids[r.ID] {
			t.Errorf("规则 ID 重复: %s", r.ID)
		}
		ids[r.ID] = true
	}
}

func TestRemovedCapabilityMessage(t *testing.T) {
	msg := RemovedCapabilityMessage("session/fork")
	if !strings.Contains(msg, "session/fork") {
		t.Errorf("文案未包含方法名: %q", msg)
	}
	if !IsMethodNotFound(-32601) || IsMethodNotFound(-32600) {
		t.Error("IsMethodNotFound 判定错误")
	}
}

// TestRewriteOtherMethodsPassThrough 验证非规则方法的参数原样通过。
func TestRewriteOtherMethodsPassThrough(t *testing.T) {
	r := Rewriter{Timeout: DefaultTimeouts()}
	in := map[string]any{"sessionId": "s", "content": "hello"}
	out, timeout, err := r.Rewrite("session/send", in)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Errorf("send 参数被意外改写: %v", out)
	}
	if timeout != DefaultTimeouts().Send {
		t.Errorf("send 超时 = %s", timeout)
	}
}

func TestMain(m *testing.M) {
	// 防止宿主环境的同名变量干扰 envDur 测试。
	os.Unsetenv("ZCODE_ACP_CREATE_TIMEOUT")
	os.Unsetenv("ZCODE_ACP_TURN_TIMEOUT")
	os.Exit(m.Run())
}
