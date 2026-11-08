package bridge

import (
	"context"
	"maps"
	"time"

	"github.com/tizerluo/zcode-open-bridge-go/internal/jsonrpc"
)

// permOutcome 是 ACP session/request_permission 的应答
// (RequestPermissionOutcome = "cancelled" | {outcome:"selected", optionId})。
//
// 之前 askOne 与 handlePermission 各自内联了一份完全相同的结构体 —— 两条路径
// 往客户端发的是同一种请求, 就该有一处解析。
type permOutcome struct {
	Outcome struct {
		Outcome  string `json:"outcome"`
		OptionID string `json:"optionId"`
	} `json:"outcome"`
}

// permCancelled 是 ACP 里「这次授权作废了、什么都没批」的规定说法。
// 放弃等待时用它收尾, 而不是让客户端那条请求悬着。
var permCancelled = map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}

// askClientRaw 向 ACP 客户端发一条请求并等应答, 超时上限 br.timeouts.Question
// (ctx 取消同样提前返回)。
//
// 协商 cancelRequest 的客户端收到 $/cancel_request, 由 SDK 取消 inbound
// callback context。未协商的旧客户端保留 T02 的反向结果帧兼容行为。
//
// 问答的两条路径 (session/request_permission 降级、elicitation/create 协商)
// 共享这一个出口, 所以取消/超时/断连的语义只有一处, 不会两边跑偏。
func (br *Bridge) askClientRaw(ctx context.Context, method string, params map[string]any, out, abandoned any) *jsonrpc.Error {
	askCtx, cancel := context.WithTimeout(ctx, br.timeouts.Question)
	defer cancel()
	br.mu.Lock()
	cancelRequest := br.clientCaps.cancelRequest
	br.mu.Unlock()
	if cancelRequest {
		params = maps.Clone(params)
		meta, _ := params["_meta"].(map[string]any)
		meta = maps.Clone(meta)
		if meta == nil {
			meta = map[string]any{}
		}
		deadline, _ := askCtx.Deadline()
		meta["_zcode"] = map[string]any{"expiresAt": deadline.UTC().Format(time.RFC3339Nano)}
		params["_meta"] = meta
	}
	return br.conn.RequestAbandonable(askCtx, method, params, out,
		func(id string) {
			if cancelRequest {
				if err := br.conn.Notify("$/cancel_request", map[string]any{"requestId": id}); err != nil {
					br.logger("  [interact] cancel notification for %s failed: %v", id, err)
				}
				return
			}
			if err := br.conn.ReplyResult(id, abandoned); err != nil {
				br.logger("  [interact] 给客户端 %s 补放弃应答失败: %v", id, err)
				return
			}
			br.logger("  [interact] 已放弃等待 %s, 给客户端 %s 回放弃应答", method, id)
		})
}

// askClient 是 session/request_permission 的专用出口: 放弃时回 ACP 规定的
// cancelled (permCancelled)。
func (br *Bridge) askClient(ctx context.Context, params map[string]any, out *permOutcome) *jsonrpc.Error {
	return br.askClientRaw(ctx, "session/request_permission", params, out, permCancelled)
}
