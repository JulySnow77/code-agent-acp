// Command workbuddy-acp-bridge is an ACP v1 stdio server backed by the
// WorkBuddy PC client's local HTTP/SSE gateway.
package main

import (
	"os"

	"workbuddy-acp-bridge/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
