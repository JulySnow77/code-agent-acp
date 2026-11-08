package acp

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

type closeCounter struct{ closed bool }

func (c *closeCounter) Close() error { c.closed = true; return nil }

func TestLineTransportFraming(t *testing.T) {
	input := "\n" + `{"jsonrpc":"2.0","id":1,"method":"x"}` + "\n\n" +
		`{"jsonrpc":"2.0","method":"y","params":{"text":"a\nb 世界"}}` + "\n"
	var out bytes.Buffer
	closer := &closeCounter{}
	tr := NewLineTransport(strings.NewReader(input), &out, closer)

	first, err := tr.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(first), `"method":"x"`) {
		t.Fatalf("blank line not skipped: %s", first)
	}
	second, err := tr.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(second), `世界`) || strings.Contains(string(second), "\n") {
		t.Fatalf("bad second frame: %s", second)
	}
	if _, err := tr.Read(); !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF, got %v", err)
	}

	if err := tr.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{}}`)); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out.String(), "\n") || strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("Write must append exactly one newline: %q", out.String())
	}
	if err := tr.Write([]byte("{\"a\":\n1}")); err == nil {
		t.Fatal("Write must reject a literal newline in a frame")
	}
	if err := tr.Close(); err != nil || !closer.closed {
		t.Fatalf("Close did not invoke closer: %v", err)
	}
}

func TestBufferedPairFullDuplex(t *testing.T) {
	a, b := NewBufferedPair()
	go func() { _ = a.Write([]byte("ping")) }()
	got, err := b.Read()
	if err != nil || string(got) != "ping" {
		t.Fatalf("b read %q, err=%v", got, err)
	}
	go func() { _ = b.Write([]byte("pong")) }()
	got, err = a.Read()
	if err != nil || string(got) != "pong" {
		t.Fatalf("a read %q, err=%v", got, err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Read(); !errors.Is(err, io.EOF) {
		t.Fatalf("closing one end must unblock the peer: %v", err)
	}
}
