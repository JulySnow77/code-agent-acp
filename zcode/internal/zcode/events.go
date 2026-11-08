package zcode

import (
	"encoding/json"
	"strconv"
)

// Event 是 session/event 通知的 params (PORTING_PLAN §2.2 C7)。
//
// ZCode 侧一律用宽类型: 协议是逆向得来且每版漂移, 未知字段必须能穿过桥而不报错。
// 翻译器按需取字段, 取不到就当没发生。
type Event struct {
	Type         string         `json:"type"`
	Payload      map[string]any `json:"payload"`
	Seq          int64          `json:"seq"`
	EventID      string         `json:"eventId"`
	SessionID    string         `json:"sessionId"`
	TurnID       string         `json:"turnId"`
	TraceID      string         `json:"traceId"`
	Timestamp    any            `json:"timestamp"`
	DeliveryKind string         `json:"deliveryKind"`
}

// ServerRequest 是 server→client 反向调用 (0.16+)。
//
// 实测: session/create 与 session/send 期间服务端会反向请求
// session/requestRuntimePreferences (scope: runtime-materialization /
// user-execution), 拿到应答才继续。不应答则该请求永久挂起 (上游 832d647 实测)。
type ServerRequest struct {
	ID     string
	Method string
	Params map[string]any
}

// Param helpers —— 宽类型取值, 缺失返回零值, 不报错。

func str(m map[string]any, k string) string {
	v, ok := m[k]
	if !ok {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	case bool:
		return strconv.FormatBool(t)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

func num(m map[string]any, k string) float64 {
	v, ok := m[k]
	if !ok {
		return 0
	}
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return 0
		}
		return f
	case string:
		f, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return 0
		}
		return f
	}
	return 0
}

func sub(m map[string]any, k string) map[string]any {
	v, ok := m[k]
	if !ok {
		return nil
	}
	t, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	return t
}

func slice(m map[string]any, k string) []any {
	v, ok := m[k]
	if !ok {
		return nil
	}
	t, ok := v.([]any)
	if !ok {
		return nil
	}
	return t
}

// Str / Num / Sub / Slice 导出给翻译器与兼容层使用。
func Str(m map[string]any, k string) string         { return str(m, k) }
func Num(m map[string]any, k string) float64        { return num(m, k) }
func Sub(m map[string]any, k string) map[string]any { return sub(m, k) }
func Slice(m map[string]any, k string) []any        { return slice(m, k) }

// PayloadStr 取 payload 里的字符串字段。
func (e Event) PayloadStr(k string) string { return str(e.Payload, k) }

// PayloadSub 取 payload 里的子对象。
func (e Event) PayloadSub(k string) map[string]any { return sub(e.Payload, k) }

// Type 的判别: 0.16.1 实测 type 在 params 顶层, payload.type 只在
// session.updated 里作子类型出现, 故顶层优先、payload 兜底。
func (e Event) Kind() string {
	if e.Type != "" {
		return e.Type
	}
	return str(e.Payload, "type")
}
