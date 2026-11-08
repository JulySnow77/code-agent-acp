package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"minimax-acp/acp"
)

// configPair is one --set-config item. ACP config ids and values are opaque
// strings chosen by the agent; we never interpret or validate them here.
type configPair struct {
	id    string
	value string
}

// parseConfigPairs parses "id=value,id2=value2". A value may itself contain
// '='. Malformed input is reported instead of being silently dropped.
func parseConfigPairs(raw string) ([]configPair, string) {
	if strings.TrimSpace(raw) == "" {
		return nil, ""
	}
	var out []configPair
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, value, ok := strings.Cut(part, "=")
		id, value = strings.TrimSpace(id), strings.TrimSpace(value)
		if !ok || id == "" || value == "" {
			return nil, fmt.Sprintf("invalid pair %q (expected configId=value)", part)
		}
		out = append(out, configPair{id: id, value: value})
	}
	return out, ""
}

// renderSessionState prints the modes/config options the agent actually
// returned. This text goes to stderr: chat's stdout belongs to the human user.
func renderSessionState(s acp.SessionStateResult) string {
	var b strings.Builder
	fmt.Fprintln(&b, "session state:")
	if s.Modes == nil {
		fmt.Fprintln(&b, "  modes: (not advertised by this agent)")
	} else {
		fmt.Fprintf(&b, "  currentModeId: %s\n", s.Modes.CurrentModeID)
		for _, m := range s.Modes.AvailableModes {
			fmt.Fprintf(&b, "  mode: id=%s name=%s %s\n", m.ID, m.Name, m.Description)
		}
	}
	if len(s.ConfigOptions) == 0 {
		fmt.Fprintln(&b, "  configOptions: (not advertised by this agent)")
		return b.String()
	}
	for _, o := range normalizeConfigOptions(s.ConfigOptions) {
		fmt.Fprintf(&b, "  option: id=%v name=%v currentValue=%v\n",
			o["id"], o["name"], o["currentValue"])
		if opts, ok := o["options"].([]any); ok {
			for _, v := range opts {
				if m, ok := v.(map[string]any); ok {
					fmt.Fprintf(&b, "    - value=%v name=%v\n", m["value"], m["name"])
				}
			}
		}
	}
	return b.String()
}

// normalizeConfigOptions round-trips through JSON so that options nested in
// vendor-specific shapes all become plain maps/slices for rendering.
func normalizeConfigOptions(in []acp.SessionConfigOption) []map[string]any {
	raw, err := json.Marshal(in)
	if err != nil {
		return nil
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}
