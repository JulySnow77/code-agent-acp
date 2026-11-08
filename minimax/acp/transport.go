package acp

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"sync"
)

// MaxFrameSize bounds a single stdio JSON frame. ACP has no Content-Length
// framing, so an unbounded line would be a memory-exhaustion vector.
const MaxFrameSize = 32 * 1024 * 1024

var ErrFrameTooLarge = errors.New("acp: frame exceeds MaxFrameSize")

// Transport exchanges whole JSON-RPC frames. Read returns one frame without the
// trailing newline; Write appends exactly one newline. Implementations must be
// safe for one reader goroutine and concurrent writers.
type Transport interface {
	Read() ([]byte, error)
	Write(frame []byte) error
	Close() error
}

// lineTransport implements the official ACP stdio transport: UTF-8 JSON, one
// message per line, no embedded newlines. Logging belongs on stderr.
type lineTransport struct {
	scanner *bufio.Scanner
	w       io.Writer
	closers []io.Closer
	writeMu sync.Mutex
}

// NewLineTransport builds a stdio-style transport over any reader/writer pair.
// Optional closers are invoked (in order) by Close; without them Close is a
// no-op, which is what an ACP server wants for os.Stdin/os.Stdout.
func NewLineTransport(r io.Reader, w io.Writer, closers ...io.Closer) Transport {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64*1024), MaxFrameSize)
	return &lineTransport{scanner: s, w: w, closers: closers}
}

func NewStdioTransport() Transport {
	return NewLineTransport(os.Stdin, os.Stdout)
}

func (t *lineTransport) Read() ([]byte, error) {
	for t.scanner.Scan() {
		line := bytes.TrimSpace(t.scanner.Bytes())
		if len(line) == 0 {
			continue // blank lines are not frames
		}
		return append([]byte(nil), line...), nil
	}
	if err := t.scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) || err.Error() == "bufio.Scanner: token too long" {
			return nil, ErrFrameTooLarge
		}
		return nil, err
	}
	return nil, io.EOF
}

func (t *lineTransport) Write(frame []byte) error {
	if bytes.ContainsAny(frame, "\n\r") {
		return errors.New("acp: frame contains an unescaped newline")
	}
	buf := make([]byte, 0, len(frame)+1)
	buf = append(buf, frame...)
	buf = append(buf, '\n')

	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if _, err := t.w.Write(buf); err != nil {
		return err
	}
	if f, ok := t.w.(interface{ Flush() error }); ok {
		return f.Flush()
	}
	return nil
}

func (t *lineTransport) Close() error {
	var first error
	for _, c := range t.closers {
		if c == nil {
			continue
		}
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// bufferedPair is an in-memory full-duplex transport for tests and embedding.
// Both ends share one done channel so closing either end unblocks both readers.
type bufferedPair struct {
	in   chan []byte
	out  chan []byte
	done chan struct{}
	once *sync.Once // shared by both ends; closing either end closes the pair
}

// NewBufferedPair returns two connected Transports. Writes on a appear as reads
// on b and vice versa. Frames are copied, so callers may reuse their buffers.
func NewBufferedPair() (Transport, Transport) {
	a2b, b2a := make(chan []byte, 256), make(chan []byte, 256)
	done := make(chan struct{})
	once := &sync.Once{}
	a := &bufferedPair{in: b2a, out: a2b, done: done, once: once}
	b := &bufferedPair{in: a2b, out: b2a, done: done, once: once}
	return a, b
}

func (p *bufferedPair) Read() ([]byte, error) {
	select {
	case frame := <-p.in:
		return frame, nil
	case <-p.done:
		return nil, io.EOF
	}
}

func (p *bufferedPair) Write(frame []byte) error {
	cp := append([]byte(nil), frame...)
	select {
	case p.out <- cp:
		return nil
	case <-p.done:
		return io.ErrClosedPipe
	}
}

func (p *bufferedPair) Close() error {
	p.once.Do(func() { close(p.done) })
	return nil
}
