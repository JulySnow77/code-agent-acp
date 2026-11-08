package main

import (
	"strings"
	"testing"

	"minimax-acp/acp"
)

func TestParseConfigPairs(t *testing.T) {
	pairs, bad := parseConfigPairs("")
	if bad != "" || len(pairs) != 0 {
		t.Fatalf("empty input = %#v / %q", pairs, bad)
	}
	pairs, bad = parseConfigPairs(" model=MiniMax-M2.7 , thinking=high ")
	if bad != "" || len(pairs) != 2 {
		t.Fatalf("pairs = %#v / %q", pairs, bad)
	}
	if pairs[0].id != "model" || pairs[0].value != "MiniMax-M2.7" ||
		pairs[1].id != "thinking" || pairs[1].value != "high" {
		t.Fatalf("unexpected pairs: %#v", pairs)
	}
	pairs, bad = parseConfigPairs("prompt=a=b")
	if bad != "" || len(pairs) != 1 || pairs[0].value != "a=b" {
		t.Fatalf("value containing '=' = %#v / %q", pairs, bad)
	}
}

func TestParseConfigPairsRejectsMalformed(t *testing.T) {
	for _, raw := range []string{"model", "=value", "model=", "model=x,,broken"} {
		if _, bad := parseConfigPairs(raw); bad == "" {
			t.Fatalf("expected error for %q", raw)
		}
	}
}

func TestRenderSessionState(t *testing.T) {
	s := acp.SessionStateResult{
		Modes: &acp.SessionModeState{
			CurrentModeID: "ask",
			AvailableModes: []acp.SessionMode{
				{ID: "ask", Name: "Ask", Description: "confirm each action"},
				{ID: "full", Name: "Full access"},
			},
		},
		ConfigOptions: []acp.SessionConfigOption{
			{
				"id":           "model",
				"name":         "Model",
				"currentValue": "MiniMax-M2.7",
				"options": []any{
					map[string]any{"value": "MiniMax-M3", "name": "M3"},
					map[string]any{"value": "MiniMax-M2.7", "name": "M2.7"},
				},
			},
		},
	}
	text := renderSessionState(s)
	for _, want := range []string{"currentModeId: ask", "mode: id=full", "option: id=model", "MiniMax-M2.7", "value=MiniMax-M3"} {
		if !strings.Contains(text, want) {
			t.Fatalf("render missing %q:\n%s", want, text)
		}
	}

	empty := renderSessionState(acp.SessionStateResult{})
	if !strings.Contains(empty, "not advertised") {
		t.Fatalf("empty state = %q", empty)
	}
}
