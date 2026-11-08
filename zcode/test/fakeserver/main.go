// Command fakeserver 是 Go 版可编程假 zcode app-server (PORTING_PLAN §3.1
// test/fakeserver; 替代上游 Python fake, CI 无 Python 依赖)。
//
// 用法 (由测试经 ZCODE_BIN 注入):
//
//	ZCODE_BIN=<此二进制> FAKE_ZCODE_SCRIPT=<script.json> [FAKE_ZCODE_LOG=<log>]
//
// script.json 形如:
//
//	{
//	  "responses": {
//	    "session/create":  {"result": {"session": {"sessionId": "fake-1"}}},
//	    "session/send": {"result": {"accepted": true}, "serverRequests": [
//	      {"method": "interaction/requestUserInput", "params": {"questions": [...]},
//	       "then": [{"method": "session/event", "params": {"type": "turn.completed", "payload": {...}}}]}
//	    ]}
//	  },
//	  "default": {"error": {"code": -32601, "message": "method not implemented"}}
//	}
//
// 行为:
//   - 每个收到的请求按 method 查 responses; 未命中走 default (无 default 则忽略)。
//   - action.push 在响应写完后逐条推送 (notification, 无 id)。
//   - action.serverRequests 逐个发起 server→client 反向调用 (id 形如 srv-N):
//     阻塞等桥的应答, 记录到日志 ({"id":"srv-N","reply":{...}}) 后推送 then。
//   - 每个收到的请求 (id/method/params) 追加写入 FAKE_ZCODE_LOG (JSONL),
//     供测试断言桥实际发出的参数。
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type notification struct {
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

// serverReq 是 fake 主动发起的反向调用 (等桥应答后才继续)。
type serverReq struct {
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
	Then   []notification `json:"then,omitempty"`
}

type action struct {
	Result         map[string]any `json:"result,omitempty"`
	Error          *rpcError      `json:"error,omitempty"`
	Push           []notification `json:"push,omitempty"`
	ServerRequests []serverReq    `json:"serverRequests,omitempty"`
	// DelayMs 在写响应前先睡这么久。用来把某个时序窗口撑开到确定性宽度 ——
	// 例如 session/send 慢响应时, 另一个 session/prompt 正好落在
	// 「已进 runTurn 但还没登记 br.turns」的旧窗口里。
	DelayMs int `json:"delayMs,omitempty"`
}

type script struct {
	Responses map[string]action `json:"responses"`
	Default   *action           `json:"default"`
}

func main() {
	var sc script
	if path := os.Getenv("FAKE_ZCODE_SCRIPT"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			fatalf("读脚本失败: %v", err)
		}
		if err := json.Unmarshal(raw, &sc); err != nil {
			fatalf("脚本非 JSON: %v", err)
		}
	}

	var logFile *os.File
	if path := os.Getenv("FAKE_ZCODE_LOG"); path != "" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			fatalf("打开日志失败: %v", err)
		}
		defer f.Close()
		logFile = f
	}

	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	nextSrvID := 0

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for in.Scan() {
		line := strings.TrimSpace(in.Text())
		if line == "" {
			continue
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal([]byte(line), &msg) != nil {
			continue
		}
		if logFile != nil {
			entry, _ := json.Marshal(map[string]any{
				"id": msg.ID, "method": msg.Method, "params": json.RawMessage(orEmpty(msg.Params)),
			})
			_, _ = logFile.Write(append(entry, '\n'))
			_ = logFile.Sync()
		}

		// 对反向调用的迟到应答 (无 serverRequests 在飞时到达): 无脚本逻辑, 忽略。
		if msg.Method == "" {
			continue
		}

		act, ok := sc.Responses[msg.Method]
		if !ok {
			if sc.Default == nil {
				continue
			}
			act = *sc.Default
		}
		resp := map[string]any{"id": msg.ID}
		if act.Error != nil {
			resp["error"] = act.Error
		} else {
			r := act.Result
			if r == nil {
				r = map[string]any{}
			}
			resp["result"] = r
		}
		if act.DelayMs > 0 {
			time.Sleep(time.Duration(act.DelayMs) * time.Millisecond)
		}
		writeJSON(out, resp)
		for _, n := range act.Push {
			writeJSON(out, map[string]any{"method": n.Method, "params": n.Params})
		}
		out.Flush() // 响应必须立即可见: 桥在等它, 不能等下一条输入才刷缓冲
		// 反向调用逐个发: 发请求 → 阻塞等桥应答 → 记录 → 推 then → 下一个。
		for i := range act.ServerRequests {
			sr := &act.ServerRequests[i]
			nextSrvID++
			id := fmt.Sprintf("srv-%d", nextSrvID)
			writeJSON(out, map[string]any{"jsonrpc": "2.0", "id": id, "method": sr.Method, "params": sr.Params})
			out.Flush()

			replyLine, answered := awaitReply(in, id)
			if logFile != nil {
				entry := map[string]any{"id": id, "method": sr.Method}
				if answered {
					entry["reply"] = json.RawMessage(replyLine)
				} else {
					entry["timeout"] = true // 桥没应答: 挂死缺陷的表现
				}
				bts, _ := json.Marshal(entry)
				_, _ = logFile.Write(append(bts, '\n'))
				_ = logFile.Sync()
			}
			if !answered {
				continue
			}
			for _, n := range sr.Then {
				writeJSON(out, map[string]any{"method": n.Method, "params": n.Params})
			}
			out.Flush()
		}
	}
}

// awaitReply 阻塞读后续行, 直到出现对 id 的应答; 返回应答原始行。
// 中途到达的其他行 (无关应答) 丢弃 —— 脚本场景单请求在飞, 桥不会并发发别的。
func awaitReply(in *bufio.Scanner, id string) (string, bool) {
	for in.Scan() {
		line := strings.TrimSpace(in.Text())
		if line == "" {
			continue
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal([]byte(line), &msg) != nil || msg.Method != "" {
			continue
		}
		if rawIDString(msg.ID) == id {
			return line, true
		}
	}
	return "", false
}

func rawIDString(raw json.RawMessage) string {
	s := string(raw)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

func writeJSON(w *bufio.Writer, v any) {
	bts, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = w.Write(append(bts, '\n'))
}

func orEmpty(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	return raw
}

func fatalf(format string, args ...any) {
	os.Stderr.WriteString(fmt.Sprintf("fakeserver: "+format+"\n", args...))
	os.Exit(1)
}
