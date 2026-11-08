package bridge

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/tizerluo/zcode-open-bridge-go/internal/jsonrpc"
	"github.com/tizerluo/zcode-open-bridge-go/internal/zcode"
)

// enhanceTimeout 是 prompt/enhance/start 等待 result 通知的总预算
// (ZCODE_ENHANCE_TIMEOUT 秒覆盖, 上游同默认)。
func enhanceTimeout() time.Duration {
	if s := strings.TrimSpace(getenv("ZCODE_ENHANCE_TIMEOUT")); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 120 * time.Second
}

func (br *Bridge) registerEnhance(conn *jsonrpc.Conn) {
	conn.Handle("prompt/enhance", br.handleEnhance)
	conn.Handle(aliasPrefix+"prompt/enhance", br.handleEnhance)
	conn.Handle("prompt/enhance/start", br.handleEnhanceStart)
	conn.Handle(aliasPrefix+"prompt/enhance/start", br.handleEnhanceStart)
	conn.Handle("prompt/enhance/cancel", br.handleEnhanceCancel)
	conn.Handle(aliasPrefix+"prompt/enhance/cancel", br.handleEnhanceCancel)
}

// handleEnhance: 同步增强 (阻塞返回增强文本)。
//
// 0.16 构建可能已删除 (后端 -32601 → 「当前版本已移除」), 不做本地参数校验。
func (br *Bridge) handleEnhance(ctx context.Context, id json.RawMessage, params json.RawMessage) (any, *jsonrpc.Error) {
	var p map[string]any
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, jsonrpc.Errf(-32602, "prompt/enhance params 解析失败: %v", err)
	}
	zcParams := map[string]any{
		"workspace": br.resolveWorkspace(p),
		"prompt":    stringField(p, "prompt"),
	}
	if v, ok := p["sessionId"]; ok {
		zcParams["sessionId"] = v
	}
	if v, ok := p["context"]; ok {
		zcParams["context"] = v
	}
	resp, err := br.request("prompt/enhance", zcParams)
	if err != nil {
		return nil, passthroughError("prompt/enhance", err, false)
	}
	result := resp.ResultMap()
	br.logger("prompt/enhance → ok (enhanced %d chars)", len(zcode.Str(result, "enhanced")))
	return result, nil
}

// handleEnhanceStart: 异步增强 job —— 先注册 result 等待通道再发 start
// (时序关键, 同 prompt 的「先 subscribe 再 send」), 阻塞等待 result 通知。
//
// Go 版每请求一个 goroutine, 等待期间 client 的 enhance/cancel 自然可达,
// 无需上游的 _drain_enhance_inbox。
func (br *Bridge) handleEnhanceStart(ctx context.Context, id json.RawMessage, params json.RawMessage) (any, *jsonrpc.Error) {
	var p map[string]any
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, jsonrpc.Errf(-32602, "prompt/enhance/start params 解析失败: %v", err)
	}
	requestID := stringField(p, "requestId")

	ch, err := br.backend.RegisterEnhance(requestID)
	if err != nil {
		return nil, jsonrpc.Errf(-32602, "prompt/enhance/start: %v", err)
	}
	defer br.backend.UnregisterEnhance(requestID)

	zcParams := map[string]any{
		"workspace": br.resolveWorkspace(p),
		"prompt":    stringField(p, "prompt"),
		"requestId": requestID,
	}
	if v, ok := p["sessionId"]; ok {
		zcParams["sessionId"] = v
	}
	if v, ok := p["context"]; ok {
		zcParams["context"] = v
	}
	resp, err := br.request("prompt/enhance/start", zcParams)
	if err != nil {
		return nil, passthroughError("prompt/enhance/start", err, false)
	}
	if accepted, _ := resp.ResultMap()["accepted"].(bool); !accepted {
		return nil, jsonrpc.Errf(-32603, "zcode enhance start not accepted: %v", resp.ResultMap())
	}

	waitCtx, cancel := context.WithTimeout(ctx, enhanceTimeout())
	defer cancel()
	select {
	case resultParams := <-ch:
		switch zcode.Str(resultParams, "status") {
		case "completed":
			enhanced := zcode.Str(resultParams, "enhanced")
			br.logger("prompt/enhance/start → completed (enhanced %d chars)", len(enhanced))
			return map[string]any{"enhanced": enhanced}, nil
		case "cancelled":
			br.logger("prompt/enhance/start → cancelled")
			return map[string]any{"status": "cancelled"}, nil
		default:
			errMsg := zcode.Str(resultParams, "errorMessage")
			if errMsg == "" {
				errMsg = "enhance job failed (status=" + zcode.Str(resultParams, "status") + ")"
			}
			return nil, jsonrpc.Errf(-32603, "zcode enhance failed: %s", errMsg)
		}
	case <-waitCtx.Done():
		if ctx.Err() != nil {
			return nil, jsonrpc.Errf(-32603, "prompt/enhance/start 被取消")
		}
		return nil, jsonrpc.Errf(-32603, "prompt/enhance result 等待超时 (%s)", enhanceTimeout())
	}
}

// handleEnhanceCancel: 取消进行中的增强 job。
func (br *Bridge) handleEnhanceCancel(ctx context.Context, id json.RawMessage, params json.RawMessage) (any, *jsonrpc.Error) {
	var p struct {
		RequestID string `json:"requestId"`
	}
	_ = json.Unmarshal(params, &p)
	resp, err := br.request("prompt/enhance/cancel", map[string]any{"requestId": p.RequestID})
	if err != nil {
		return nil, passthroughError("prompt/enhance/cancel", err, false)
	}
	result := resp.ResultMap()
	br.logger("prompt/enhance/cancel → cancelled=%v", result["cancelled"])
	return result, nil
}
