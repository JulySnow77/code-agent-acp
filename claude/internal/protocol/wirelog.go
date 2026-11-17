package protocol

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// WireLog is an optional JSONL recording of raw protocol frames. It is only
// enabled explicitly (CLAUDE_ACP_WIRE_LOG), because a session can contain
// credentials in tool output.
type WireLog struct {
	mu     sync.Mutex
	file   *os.File
	prefix string
}

// OpenWireLog creates <dir>/<prefix>-<timestamp>.jsonl. An empty dir disables
// recording and returns a nil logger (all methods are nil-safe).
func OpenWireLog(dir, prefix string) (*WireLog, error) {
	if dir == "" {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	name := fmt.Sprintf("%s-%s.jsonl", prefix, time.Now().Format("20060102-150405.000"))
	file, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	return &WireLog{file: file, prefix: prefix}, nil
}

// Record appends one already-decoded frame with its direction and timestamp.
func (l *WireLog) Record(direction string, frame []byte) {
	if l == nil || l.file == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	line := Message{
		"ts":        time.Now().Format(time.RFC3339Nano),
		"channel":   l.prefix,
		"direction": direction,
		"frame":     string(frame),
	}
	body, err := Encode(line)
	if err != nil {
		return
	}
	_, _ = l.file.Write(append(body, '\n'))
}

// Close flushes and closes the log file.
func (l *WireLog) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	err := l.file.Close()
	l.file = nil
	return err
}
