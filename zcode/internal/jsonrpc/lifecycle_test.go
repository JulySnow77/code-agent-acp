package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"
)

func TestConnectionEndCancelsActiveHandler(t *testing.T) {
	for _, cause := range []string{"eof", "read_error", "context"} {
		t.Run(cause, func(t *testing.T) {
			reader, writer := io.Pipe()
			defer reader.Close()
			defer writer.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			conn := New(io.Discard, nil, false)
			entered := make(chan struct{})
			conn.Handle("wait", func(ctx context.Context, _, _ json.RawMessage) (any, *Error) {
				close(entered)
				<-ctx.Done()
				return nil, nil
			})
			done := make(chan error, 1)
			go func() { done <- conn.Run(ctx, reader) }()
			if _, err := io.WriteString(writer, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"wait\"}\n"); err != nil {
				t.Fatal(err)
			}
			<-entered
			readErr := errors.New("test reader failure")
			switch cause {
			case "eof":
				writer.Close()
			case "read_error":
				writer.CloseWithError(readErr)
			case "context":
				cancel()
			}
			select {
			case err := <-done:
				switch cause {
				case "eof":
					if err != nil {
						t.Fatal(err)
					}
				case "read_error":
					if !errors.Is(err, readErr) {
						t.Fatalf("error = %v", err)
					}
				case "context":
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("error = %v", err)
					}
				}
			case <-time.After(time.Second):
				cancel()
				writer.Close()
				<-done
				t.Fatal("connection end left an active handler or reader waiting")
			}
		})
	}
}
