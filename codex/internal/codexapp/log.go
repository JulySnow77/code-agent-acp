package codexapp

import (
	"fmt"
	"os"
)

// Diagnostic writes to stderr only. ACP stdout is reserved for protocol frames.
func Diagnostic(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "codex-acp-bridge: "+format+"\n", args...)
}
