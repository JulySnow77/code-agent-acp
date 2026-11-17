package bridge

import (
	"os"
)

// writeTempConfig persists a JSON config for the CLI's --mcp-config flag.
// Mode 0600 because MCP env blocks can contain credentials.
func writeTempConfig(body []byte) (string, error) {
	file, err := os.CreateTemp("", "claude-acp-mcp-*.json")
	if err != nil {
		return "", err
	}
	name := file.Name()
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

func removeTempConfig(path string) {
	if path != "" {
		_ = os.Remove(path)
	}
}
