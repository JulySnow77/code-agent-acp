package acp_test

import (
	"context"
	"encoding/json"
	"fmt"

	"kimi-acp/acp"
)

// ExampleConn shows that a Conn is symmetric: either peer can serve a method.
// This is what makes an ACP agent able to call session/request_permission or
// fs/read_text_file back into the client during a prompt turn.
func ExampleConn() {
	aTr, bTr := acp.NewBufferedPair()
	a, b := acp.NewConn(aTr), acp.NewConn(bTr)
	go func() { _ = a.Run(context.Background()) }()
	go func() { _ = b.Run(context.Background()) }()
	defer func() { a.Close(); b.Close() }()

	b.OnRequest("double", func(_ context.Context, raw json.RawMessage) (any, *acp.RPCError) {
		var p struct {
			N int `json:"n"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, acp.InvalidParams("%v", err)
		}
		return map[string]any{"v": 2 * p.N}, nil
	})

	var out struct {
		V int `json:"v"`
	}
	err := a.Call(context.Background(), "double", map[string]any{"n": 21}, &out)
	fmt.Println(out.V, err)
	// Output: 42 <nil>
}
