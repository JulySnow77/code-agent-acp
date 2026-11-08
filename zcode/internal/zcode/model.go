package zcode

import (
	"fmt"
	"strings"
)

// ModelRef 是 zcode 的模型引用 {providerId, modelId}。
type ModelRef struct {
	ProviderID string `json:"providerId"`
	ModelID    string `json:"modelId"`
}

func (r ModelRef) String() string {
	if r.ProviderID == "" {
		return r.ModelID
	}
	return r.ProviderID + "/" + r.ModelID
}

// ModelEntry 是 session/create 返回的 model.available[] 中的一项 (§2.2 C3)。
//
// 实测形状 (0.16.5, testdata/zcode-0.16.5/session-create.response.json):
//
//	{"ref":{"providerId","modelId"}, "label", "providerLabel", "contextWindow",
//	 "maxOutputTokens", "properties":{...},
//	 "reasoning":{"defaultLevel":"max","levels":[{"label":"disabled","value":"disabled"}, ...]}}
//
// 注意 reasoning.levels 是 {label,value} 对象数组而非字符串数组 —— 计划 §2.2 C3
// 只写了 levels[], 按字符串解析会得到空列表, 进而让 setModel 因缺
// reasoningLevel 被拒 (实测 "Reasoning level is required")。
type ModelEntry struct {
	Ref           ModelRef `json:"ref"`
	Label         string   `json:"label"`
	ProviderLabel string   `json:"providerLabel"`
	ContextWindow int      `json:"contextWindow"`
	Levels        []string `json:"levels"` // reasoning.levels[] 的 value
	DefaultLevel  string   `json:"defaultLevel"`
}

// ModelCatalog 是建会话时缓存下来的模型目录。
//
// 用途: 把旧格式的 {modelId} 解析成 0.16.5 要求的
// {model:{providerId,modelId,options:{reasoningLevel}}} (§2.2 C4),
// 以及 doctor 提前报「所选模型不在目录中」。
type ModelCatalog struct {
	Entries []ModelEntry
}

// ParseModelCatalog 从 session/create 的 result 解析模型目录。
// 目录缺失不是错误 (某些模式不返回 settings), 返回空目录。
func ParseModelCatalog(result map[string]any) *ModelCatalog {
	c := &ModelCatalog{}
	settings, _ := result["settings"].(map[string]any)
	if settings == nil {
		return c
	}
	model, _ := settings["model"].(map[string]any)
	if model == nil {
		return c
	}
	avail, _ := model["available"].([]any)
	for _, raw := range avail {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		e := ModelEntry{
			Label:         Str(m, "label"),
			ProviderLabel: Str(m, "providerLabel"),
			ContextWindow: int(Num(m, "contextWindow")),
		}
		if ref, ok := m["ref"].(map[string]any); ok {
			e.Ref = ModelRef{ProviderID: Str(ref, "providerId"), ModelID: Str(ref, "modelId")}
		} else {
			// 兜底: 某些版本把 providerId/modelId 平铺在条目上。
			e.Ref = ModelRef{ProviderID: Str(m, "providerId"), ModelID: Str(m, "modelId")}
		}
		if reasoning, ok := m["reasoning"].(map[string]any); ok {
			e.DefaultLevel = Str(reasoning, "defaultLevel")
			if levels, ok := reasoning["levels"].([]any); ok {
				for _, l := range levels {
					if v := levelValue(l); v != "" {
						e.Levels = append(e.Levels, v)
					}
				}
			}
		}
		c.Entries = append(c.Entries, e)
	}
	return c
}

// Empty 报告目录是否为空。
func (c *ModelCatalog) Empty() bool { return c == nil || len(c.Entries) == 0 }

// Find 按 providerId/modelId 精确匹配; 两者都空返回 nil。
func (c *ModelCatalog) Find(providerID, modelID string) *ModelEntry {
	if c == nil {
		return nil
	}
	for i := range c.Entries {
		e := &c.Entries[i]
		if providerID != "" && e.Ref.ProviderID != providerID {
			continue
		}
		if modelID != "" && e.Ref.ModelID != modelID {
			continue
		}
		return e
	}
	return nil
}

// Resolve 把 "providerId/modelId" 或 "modelId" 解析成完整引用。
// 只给 modelId 时, 目录里唯一匹配则用它, 多个匹配返回歧义错误。
func (c *ModelCatalog) Resolve(spec string) (ModelRef, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return ModelRef{}, fmt.Errorf("模型为空")
	}
	providerID, modelID := "", spec
	if i := strings.Index(spec, "/"); i >= 0 {
		providerID, modelID = spec[:i], spec[i+1:]
	}
	if c == nil || len(c.Entries) == 0 {
		// 无目录: 只能原样透传, 由 zcode 判定。
		return ModelRef{ProviderID: providerID, ModelID: modelID}, nil
	}
	if providerID != "" {
		if e := c.Find(providerID, modelID); e != nil {
			return e.Ref, nil
		}
	} else {
		// 裸 modelId: 必须全目录扫描 —— 多个 provider 挂同名模型时
		// 静默取第一个会选错模型 (实测 0.16.5 目录里 gpt-5.5 有两个)。
		var match *ModelEntry
		for i := range c.Entries {
			e := &c.Entries[i]
			if e.Ref.ModelID != modelID {
				continue
			}
			if match != nil {
				return ModelRef{}, fmt.Errorf("模型 %q 在目录中有多个 provider (%s, %s…), 请用 providerId/modelId 指定",
					spec, match.Ref.ProviderID, e.Ref.ProviderID)
			}
			match = e
		}
		if match != nil {
			return match.Ref, nil
		}
	}
	var hints []string
	for i := range c.Entries {
		if len(hints) >= 8 {
			break
		}
		hints = append(hints, c.Entries[i].Ref.String())
	}
	return ModelRef{}, fmt.Errorf("模型 %q 不在当前会话的模型目录中 (可用: %s)",
		spec, strings.Join(hints, ", "))
}

// levelValue 从 reasoning.levels[] 的一项里取值。
// 兼容两种形状: 字符串 (旧版) 与 {label,value} 对象 (0.16.5 实测)。
func levelValue(raw any) string {
	switch t := raw.(type) {
	case string:
		return t
	case map[string]any:
		for _, k := range []string{"value", "id", "name", "level"} {
			if v := Str(t, k); v != "" {
				return v
			}
		}
		return Str(t, "label")
	}
	return ""
}

// DefaultReasoningLevel 返回某模型应使用的缺省推理等级。
//
// 0.16.5 的 setModel 对部分模型要求 reasoningLevel 必填 (实测
// "Reasoning level is required for <provider>/<model>"), 故必须在目录里
// 查到可用等级。优先用服务端给的 reasoning.defaultLevel, 其次第一个等级。
func (c *ModelCatalog) DefaultReasoningLevel(ref ModelRef) string {
	if c == nil {
		return ""
	}
	e := c.Find(ref.ProviderID, ref.ModelID)
	if e == nil {
		return ""
	}
	if e.DefaultLevel != "" {
		return e.DefaultLevel
	}
	if len(e.Levels) > 0 {
		return e.Levels[0]
	}
	return ""
}
