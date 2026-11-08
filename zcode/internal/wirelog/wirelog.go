// Package wirelog 提供线材日志: 把桥两侧的关键往来以 JSON Lines 追加落盘。
//
// 对应上游的 wire_log(), 默认关闭 (ZCODE_ACP_WIRE_LOG 为空)。用于排查
// 「答案没传到 agent / 问题重复显示」这类只在真实时序下才出现的问题。
// 写入失败一律静默: 日志绝不能拖垮协议流。
package wirelog

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// Recorder 是单个线材日志文件。零值 Recorder 是安全的空记录器 (所有写入被丢弃)。
type Recorder struct {
	mu   sync.Mutex
	f    *os.File
	path string
}

// New 打开 path 追加写。path 为空返回 nil (关闭状态)。
func New(path string) *Recorder {
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil
	}
	return &Recorder{f: f, path: path}
}

// Path 返回日志路径 (关闭时为空串)。
func (r *Recorder) Path() string {
	if r == nil {
		return ""
	}
	return r.path
}

// Log 追加一条记录。dir 建议取 "zcode→bridge" / "bridge→zcode" /
// "bridge→acp" / "acp→bridge"。
func (r *Recorder) Log(dir, kind string, msg any) {
	if r == nil || r.f == nil {
		return
	}
	rec := struct {
		T    float64 `json:"t"`
		Dir  string  `json:"dir"`
		Kind string  `json:"kind"`
		Msg  any     `json:"msg"`
	}{
		T:    float64(time.Now().UnixNano()) / 1e9,
		Dir:  dir,
		Kind: kind,
		Msg:  msg,
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	b = append(b, '\n')
	r.mu.Lock()
	defer r.mu.Unlock()
	_, _ = r.f.Write(b)
}

// Close 关闭日志文件。
func (r *Recorder) Close() {
	if r == nil || r.f == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.f.Close()
	r.f = nil
}
